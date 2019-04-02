package node

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/tokenized/smart-contract/internal/platform/wallet"
	"github.com/tokenized/smart-contract/pkg/inspector"
	"github.com/tokenized/smart-contract/pkg/logger"
	"github.com/tokenized/smart-contract/pkg/protocol"
	"github.com/tokenized/smart-contract/pkg/txbuilder"
	"github.com/tokenized/smart-contract/pkg/wire"

	"github.com/btcsuite/btcd/btcec"
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

	logger.Error(ctx, "%s", err)
}

// RespondReject sends a rejection message
func RespondReject(ctx context.Context, w *ResponseWriter, itx *inspector.Transaction, rk *wallet.RootKey, code uint8) error {

	// Sender is the address that sent the message that we are rejecting.
	sender := itx.Inputs[0].Address

	// Receiver (contract) is the address sending the message (UTXO)
	var contractOutput *inspector.Output
	for i, output := range itx.Outputs {
		if bytes.Equal(output.Address.ScriptAddress(), rk.Address.ScriptAddress()) {
			contractOutput = &itx.Outputs[i]
			break
		}
	}

	if contractOutput == nil {
		return errors.New("Contract output not found")
	}

	// Create reject tx
	rejectTx := txbuilder.NewTx(contractOutput.Address.ScriptAddress(), w.Config.DustLimit, w.Config.FeeRate)

	// Find spendable UTXOs
	utxos, err := itx.UTXOs().ForAddress(contractOutput.Address)
	if err != nil {
		Error(ctx, w, ErrInsufficientFunds)
		return ErrNoResponse
	}

	for _, utxo := range utxos {
		rejectTx.AddInput(wire.OutPoint{Hash: utxo.Hash, Index: utxo.Index}, utxo.PkScript, uint64(utxo.Value))
	}

	// Add a dust output to the sender, but so they will also receive change.
	rejectTx.AddP2PKHDustOutput(sender.ScriptAddress(), true)

	rejectionCodes, err := protocol.GetRejectionCodes()
	if err != nil {
		Error(ctx, w, err)
		return ErrNoResponse
	}

	rejectionCode, exists := rejectionCodes[code]
	if !exists {
		Error(ctx, w, fmt.Errorf("Rejection code %d not found", code))
		return ErrNoResponse
	}

	// Build rejection
	rejection := protocol.Rejection{
		RejectionType:  code,
		MessagePayload: string(rejectionCode.Text),
	}

	// Add the rejection payload
	payload, err := protocol.Serialize(&rejection)
	if err != nil {
		Error(ctx, w, err)
		return ErrNoResponse
	}
	rejectTx.AddOutput(payload, 0, false, false)

	// Sign the tx
	err = rejectTx.Sign([]*btcec.PrivateKey{rk.PrivateKey})
	if err != nil {
		if txbuilder.IsErrorCode(err, txbuilder.ErrorCodeInsufficientValue) {
			logger.Warn(ctx, err.Error())
			return ErrNoResponse
		} else {
			Error(ctx, w, err)
			return ErrNoResponse
		}
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
	respondTx := txbuilder.NewTx(rk.Address.ScriptAddress(), w.Config.DustLimit, w.Config.FeeRate)

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
	payload, err := protocol.Serialize(msg)
	if err != nil {
		Error(ctx, w, err)
		return ErrNoResponse
	}
	respondTx.AddOutput(payload, 0, false, false)

	// Sign the tx
	err = respondTx.Sign([]*btcec.PrivateKey{rk.PrivateKey})
	if err != nil {
		if txbuilder.IsErrorCode(err, txbuilder.ErrorCodeInsufficientValue) {
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
