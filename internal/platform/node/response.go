package node

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/tokenized/smart-contract/internal/platform/wallet"
	"github.com/tokenized/smart-contract/pkg/inspector"
	"github.com/tokenized/smart-contract/pkg/logger"
	"github.com/tokenized/smart-contract/pkg/txbuilder"
	"github.com/tokenized/smart-contract/pkg/wire"
	"github.com/tokenized/specification/dist/golang/protocol"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil"
)

var (
	// ErrSystemError occurs for a non standard response.
	ErrSystemError = errors.New("System error")

	// ErrNoResponse occurs when there is no response.
	ErrNoResponse = errors.New("No response given")

	// ErrRejected occurs for a rejected response.
	ErrRejected = errors.New("Transaction rejected")

	// ErrInsufficientFunds occurs for a poorly funded request.
	ErrInsufficientFunds = errors.New("Insufficient Payment amount")
)

// Error handles all error responses for the API.
func Error(ctx context.Context, w *ResponseWriter, err error) {
	// switch errors.Cause(err) {
	// }

	LogDepth(ctx, logger.LevelError, 1, "%s", err)
}

// RespondReject sends a rejection message.
// If no reject output data is specified, then the remainder is sent to the PKH of the first input.
// Since most bitcoins sent to the contract are just for response tx fee funding, this isn't a real
//   issue.
// The scenario in which it is important is when there is a multi-party transfer involving
//   bitcoins. In this scenario inputs send bitcoins to the smart contract to distribute to
//   receivers based on the transfer request data. We will need to analyze the transfer request
//   data to determine which inputs were to have funded sending bitcoins, and return the bitcoins
//   to them.
func RespondReject(ctx context.Context, w *ResponseWriter, itx *inspector.Transaction, rk *wallet.RootKey, code uint8) error {
	rejectionCode := protocol.GetRejectionCode(code)
	if rejectionCode == nil {
		Error(ctx, w, fmt.Errorf("Rejection code %d not found", code))
		return ErrNoResponse
	}

	v := ctx.Value(KeyValues).(*Values)

	// Build rejection
	rejection := protocol.Rejection{
		RejectionCode: code,
		Message:       rejectionCode.Label,
		Timestamp:     v.Now,
	}

	// Contract address
	contractAddress := rk.Address

	// Find spendable UTXOs
	var utxos []inspector.UTXO
	var err error
	if len(w.RejectInputs) > 0 {
		utxos = w.RejectInputs // Custom UTXOs. Just refund anything available to them.
	} else {
		utxos, err = itx.UTXOs().ForAddress(contractAddress)
		if err != nil {
			Error(ctx, w, err)
			return ErrNoResponse
		}
	}

	if len(utxos) == 0 {
		Error(ctx, w, errors.New("Contract UTXOs not found"))
		return ErrNoResponse // Contract UTXOs not found
	}

	// Create reject tx. Change goes back to requestor.
	var rejectTx *txbuilder.Tx
	if len(w.RejectOutputs) > 0 {
		var changeAddress btcutil.Address
		for _, output := range w.RejectOutputs {
			if output.Change {
				changeAddress = output.Address
				break
			}
		}
		if changeAddress == nil {
			changeAddress = w.RejectOutputs[0].Address
		}
		rejectTx = txbuilder.NewTx(changeAddress.ScriptAddress(), w.Config.DustLimit, w.Config.FeeRate)
	} else {
		rejectTx = txbuilder.NewTx(itx.Inputs[0].Address.ScriptAddress(), w.Config.DustLimit, w.Config.FeeRate)
	}

	for _, utxo := range utxos {
		rejectTx.AddInput(wire.OutPoint{Hash: utxo.Hash, Index: utxo.Index}, utxo.PkScript, uint64(utxo.Value))
	}

	// Add a dust output to the requestor, but so they will also receive change.
	if len(w.RejectOutputs) > 0 {
		rejectAddressFound := false
		for i, output := range w.RejectOutputs {
			if output.Value < w.Config.DustLimit {
				output.Value = w.Config.DustLimit
			}
			rejectTx.AddP2PKHOutput(output.Address.ScriptAddress(), output.Value, output.Change)
			rejection.AddressIndexes = append(rejection.AddressIndexes, uint16(i))
			if w.RejectPKH != nil && bytes.Equal(output.Address.ScriptAddress(), w.RejectPKH.Bytes()) {
				rejectAddressFound = true
				rejection.RejectAddressIndex = uint16(i)
			}
		}
		if !rejectAddressFound && w.RejectPKH != nil {
			rejection.AddressIndexes = append(rejection.AddressIndexes, uint16(len(rejectTx.Outputs)))
			rejectTx.AddP2PKHDustOutput(w.RejectPKH.Bytes(), false)
		}
	} else {
		// Give it all back to the first input. This is the common scenario when the first input is
		//   the only requestor involved.
		rejectTx.AddP2PKHDustOutput(itx.Inputs[0].Address.ScriptAddress(), true)
		rejection.AddressIndexes = append(rejection.AddressIndexes, 0)
		rejection.RejectAddressIndex = 0
	}

	// Add the rejection payload
	payload, err := protocol.Serialize(&rejection, w.Config.IsTest)
	if err != nil {
		Error(ctx, w, err)
		return ErrNoResponse
	}
	rejectTx.AddOutput(payload, 0, false, false)

	// Sign the tx
	err = rejectTx.Sign([]*btcec.PrivateKey{rk.PrivateKey})
	if err != nil {
		Error(ctx, w, err)
		return ErrNoResponse
	}

	if err := Respond(ctx, w, rejectTx.MsgTx); err != nil {
		Error(ctx, w, err)
		return ErrNoResponse
	}
	return ErrRejected
}

// RespondSuccess broadcasts a successful message
func RespondSuccess(ctx context.Context, w *ResponseWriter, itx *inspector.Transaction, rk *wallet.RootKey, msg protocol.OpReturnMessage) error {

	// Create respond tx. Use contract address as backup change
	//address if an output wasn't specified
	respondTx := txbuilder.NewTx(w.Config.FeePKH.Bytes(), w.Config.DustLimit, w.Config.FeeRate)

	// Get the specified UTXOs, otherwise look up the spendable
	// UTXO's received for the contract address
	var utxos []inspector.UTXO
	var err error
	if len(w.Inputs) > 0 {
		utxos = w.Inputs
	} else {
		utxos, err = itx.UTXOs().ForAddress(rk.Address)
		if err != nil {
			Error(ctx, w, err)
			return ErrNoResponse
		}
	}

	// Add specified inputs
	for _, utxo := range utxos {
		respondTx.AddInput(wire.OutPoint{Hash: utxo.Hash, Index: utxo.Index}, utxo.PkScript, uint64(utxo.Value))
	}

	// Add specified outputs
	for _, out := range w.Outputs {
		err := respondTx.AddOutput(txbuilder.P2PKHScriptForPKH(out.Address.ScriptAddress()), out.Value, out.Change, false)
		if err != nil {
			Error(ctx, w, err)
			return ErrNoResponse
		}
	}

	// Add the payload
	payload, err := protocol.Serialize(msg, w.Config.IsTest)
	if err != nil {
		Error(ctx, w, err)
		return ErrNoResponse
	}
	respondTx.AddOutput(payload, 0, false, false)

	// Sign the tx
	err = respondTx.Sign([]*btcec.PrivateKey{rk.PrivateKey})
	if err != nil {
		if txbuilder.IsErrorCode(err, txbuilder.ErrorCodeInsufficientValue) {
			LogWarn(ctx, "Sending reject. Failed to sign tx : %s", err)
			return RespondReject(ctx, w, itx, rk, protocol.RejectInsufficientTxFeeFunding)
		} else {
			Error(ctx, w, err)
			return ErrNoResponse
		}
	}

	return Respond(ctx, w, respondTx.MsgTx)
}

// Respond sends a TX to the network.
func Respond(ctx context.Context, w *ResponseWriter, tx *wire.MsgTx) error {
	return w.Respond(ctx, tx)
}