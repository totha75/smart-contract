package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/tokenized/smart-contract/cmd/smartcontractd/listeners"
	"github.com/tokenized/smart-contract/internal/asset"
	"github.com/tokenized/smart-contract/internal/contract"
	"github.com/tokenized/smart-contract/internal/platform/db"
	"github.com/tokenized/smart-contract/internal/platform/node"
	"github.com/tokenized/smart-contract/internal/platform/state"
	"github.com/tokenized/smart-contract/internal/platform/wallet"
	"github.com/tokenized/smart-contract/internal/transactions"
	"github.com/tokenized/smart-contract/internal/transfer"
	"github.com/tokenized/smart-contract/internal/utxos"
	"github.com/tokenized/smart-contract/pkg/inspector"
	"github.com/tokenized/smart-contract/pkg/logger"
	"github.com/tokenized/smart-contract/pkg/scheduler"
	"github.com/tokenized/smart-contract/pkg/txbuilder"
	"github.com/tokenized/smart-contract/pkg/txscript"
	"github.com/tokenized/smart-contract/pkg/wire"
	"github.com/tokenized/specification/dist/golang/protocol"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcutil"
	"github.com/pkg/errors"
	"go.opencensus.io/trace"
	"golang.org/x/crypto/ripemd160"
)

type Message struct {
	MasterDB  *db.DB
	Config    *node.Config
	Headers   BitcoinHeaders
	Tracer    *listeners.Tracer
	Scheduler *scheduler.Scheduler
	UTXOs     *utxos.UTXOs
}

// ProcessMessage handles an incoming Message OP_RETURN.
func (m *Message) ProcessMessage(ctx context.Context, w *node.ResponseWriter, itx *inspector.Transaction, rk *wallet.RootKey) error {
	ctx, span := trace.StartSpan(ctx, "handlers.Message.ProcessMessage")
	defer span.End()

	v := ctx.Value(node.KeyValues).(*node.Values)

	msg, ok := itx.MsgProto.(*protocol.Message)
	if !ok {
		return errors.New("Could not assert as *protocol.Message")
	}

	// Check if message is addressed to contract.
	found := false
	for _, outputIndex := range msg.AddressIndexes {
		if int(outputIndex) >= len(itx.Outputs) {
			return fmt.Errorf("Message output index out of range : %d/%d", outputIndex, len(itx.Outputs))
		}

		if bytes.Equal(rk.Address.ScriptAddress(), itx.Outputs[outputIndex].Address.ScriptAddress()) {
			found = true
			break
		}
	}

	if !found {
		return nil // Message not addressed to contract.
	}

	// Validate all fields have valid values.
	if err := msg.Validate(); err != nil {
		logger.Warn(ctx, "%s : Message invalid : %s", v.TraceID, err)
		return node.RespondReject(ctx, w, itx, rk, protocol.RejectMsgMalformed)
	}

	messagePayload := protocol.MessageTypeMapping(msg.MessageType)
	if messagePayload == nil {
		return fmt.Errorf("Unknown message payload type : %04d", msg.MessageType)
	}

	_, err := messagePayload.Write(msg.MessagePayload)
	if err != nil {
		logger.Warn(ctx, "%s : Failed to parse message payload : %s", v.TraceID, err)
		return node.RespondReject(ctx, w, itx, rk, protocol.RejectMsgMalformed)
	}

	if err := messagePayload.Validate(); err != nil {
		logger.Warn(ctx, "%s : Message %d payload is invalid : %s", v.TraceID, msg.MessageType, err)
		return node.RespondReject(ctx, w, itx, rk, protocol.RejectMsgMalformed)
	}

	switch payload := messagePayload.(type) {
	case *protocol.SettlementRequest:
		logger.Verbose(ctx, "%s : Processing Settlement Request", v.TraceID)
		return m.processSettlementRequest(ctx, w, itx, payload, rk)
	case *protocol.SignatureRequest:
		logger.Verbose(ctx, "%s : Processing Signature Request", v.TraceID)
		return m.processSigRequest(ctx, w, itx, payload, rk)
	default:
		return fmt.Errorf("Unknown message payload type : %04d", msg.MessageType)
	}
}

// ProcessRejection handles an incoming Rejection OP_RETURN.
func (m *Message) ProcessRejection(ctx context.Context, w *node.ResponseWriter, itx *inspector.Transaction, rk *wallet.RootKey) error {
	ctx, span := trace.StartSpan(ctx, "handlers.Message.ProcessRejection")
	defer span.End()

	msg, ok := itx.MsgProto.(*protocol.Rejection)
	if !ok {
		return errors.New("Could not assert as *protocol.Rejection")
	}

	// Check if message is addressed to this contract.
	found := false
	for _, outputIndex := range msg.AddressIndexes {
		if int(outputIndex) >= len(itx.Outputs) {
			return fmt.Errorf("Reject message output index out of range : %d/%d", outputIndex, len(itx.Outputs))
		}

		if bytes.Equal(rk.Address.ScriptAddress(), itx.Outputs[outputIndex].Address.ScriptAddress()) {
			found = true
			break
		}
	}

	if !found {
		return nil // Message not addressed to this contract.
	}

	// Validate all fields have valid values.
	if err := msg.Validate(); err != nil {
		return errors.Wrap(err, "Invalid rejection tx")
	}

	logger.Warn(ctx, "Rejection received (%d) : %s", msg.RejectionCode, msg.Message)

	// Trace back to original request tx if necessary.
	hash := m.Tracer.Retrace(ctx, itx.MsgTx)
	var problemTx *inspector.Transaction
	var err error
	if hash != nil {
		problemTx, err = transactions.GetTx(ctx, m.MasterDB, hash, &m.Config.ChainParams, m.Config.IsTest)
	} else {
		problemTx, err = transactions.GetTx(ctx, m.MasterDB, &itx.Inputs[0].UTXO.Hash, &m.Config.ChainParams, m.Config.IsTest)
	}
	if err != nil {
		return nil
	}

	switch problemMsg := problemTx.MsgProto.(type) {
	case *protocol.Transfer:
		// Refund any funds from the transfer tx that were sent to the this contract.
		return refundTransferFromReject(ctx, m.MasterDB, m.Scheduler, m.Config, w, itx, msg, problemTx, problemMsg, rk)

	default:
	}

	return nil
}

// ProcessRevert handles a tx that has been reverted either through a reorg or zero conf double spend.
func (m *Message) ProcessRevert(ctx context.Context, w *node.ResponseWriter, itx *inspector.Transaction, rk *wallet.RootKey) error {
	ctx, span := trace.StartSpan(ctx, "handlers.Message.ProcessRevert")
	defer span.End()

	// Serialize tx for Message OP_RETURN.
	var buf bytes.Buffer
	err := itx.MsgTx.Serialize(&buf)
	if err != nil {
		return errors.Wrap(err, "Failed to serialize revert tx")
	}

	v := ctx.Value(node.KeyValues).(*node.Values)

	messagePayload := protocol.RevertedTx{
		Version:     0,
		Timestamp:   v.Now,
		Transaction: buf.Bytes(),
	}

	// Setup Message
	var data []byte
	data, err = messagePayload.Serialize()
	if err != nil {
		return errors.Wrap(err, "Failed to serialize revert payload")
	}
	message := protocol.Message{
		AddressIndexes: []uint16{0}, // First receiver is issuer
		MessageType:    messagePayload.Type(),
		MessagePayload: data,
	}

	contractPKH := protocol.PublicKeyHashFromBytes(rk.Address.ScriptAddress())
	ct, err := contract.Retrieve(ctx, m.MasterDB, contractPKH)
	if err != nil {
		return errors.Wrap(err, "Failed to retrieve contract")
	}

	// Create tx
	tx := txbuilder.NewTx(contractPKH.Bytes(), m.Config.DustLimit, m.Config.FeeRate)

	// Add outputs to issuer/operator
	tx.AddP2PKHDustOutput(ct.IssuerPKH.Bytes(), false)
	outputAmount := uint64(m.Config.DustLimit)
	if !ct.OperatorPKH.IsZero() {
		// Add operator
		tx.AddP2PKHDustOutput(ct.OperatorPKH.Bytes(), false)
		message.AddressIndexes = append(message.AddressIndexes, uint16(1))
		outputAmount += uint64(m.Config.DustLimit)
	}

	// Serialize payload
	payload, err := protocol.Serialize(&message, m.Config.IsTest)
	if err != nil {
		return errors.Wrap(err, "Failed to serialize revert message")
	}
	tx.AddOutput(payload, 0, false, false)

	// Estimate fee with 2 inputs
	amount := tx.EstimatedFee() + outputAmount + (2 * txbuilder.EstimatedInputSize)

	for {
		utxos, err := m.UTXOs.Get(amount, rk.Address.ScriptAddress())
		if err != nil {
			return errors.Wrap(err, "Failed to get UTXOs")
		}

		for _, utxo := range utxos {
			if err := tx.AddInput(utxo.OutPoint, utxo.Output.PkScript, uint64(utxo.Output.Value)); err != nil {
				return errors.Wrap(err, "Failed add input")
			}
		}

		err = tx.Sign([]*btcec.PrivateKey{rk.PrivateKey})
		if err == nil {
			break
		}
		if txbuilder.IsErrorCode(err, txbuilder.ErrorCodeInsufficientValue) {
			// Get more utxos
			amount = uint64(float32(amount) * 1.25)
			utxos, err = m.UTXOs.Get(amount, rk.Address.ScriptAddress())
			if err != nil {
				return errors.Wrap(err, "Failed to get UTXOs")
			}

			// Clear inputs
			tx.Inputs = nil
		}
	}

	// Send tx
	return node.Respond(ctx, w, tx.MsgTx)
}

// processSettlementRequest handles an incoming Message SettlementRequest payload.
func (m *Message) processSettlementRequest(ctx context.Context, w *node.ResponseWriter,
	itx *inspector.Transaction, settlementRequest *protocol.SettlementRequest, rk *wallet.RootKey) error {
	ctx, span := trace.StartSpan(ctx, "handlers.Message.processSettlementRequest")
	defer span.End()

	opReturn, err := protocol.Deserialize(settlementRequest.Settlement, m.Config.IsTest)
	if err != nil {
		return errors.Wrap(err, "Failed to deserialize settlement from settlement request")
	}

	settlement, ok := opReturn.(*protocol.Settlement)
	if !ok {
		return errors.New("Settlement Request payload not a settlement")
	}

	v := ctx.Value(node.KeyValues).(*node.Values)

	// Get transfer tx
	var transferTxId *chainhash.Hash
	transferTxId, err = chainhash.NewHash(settlementRequest.TransferTxId.Bytes())
	if err != nil {
		return err
	}

	var transferTx *inspector.Transaction
	transferTx, err = transactions.GetTx(ctx, m.MasterDB, transferTxId, &m.Config.ChainParams, m.Config.IsTest)
	if err != nil {
		return errors.Wrap(err, "Transfer tx not found")
	}

	// Get transfer from it
	transfer, ok := transferTx.MsgProto.(*protocol.Transfer)
	if !ok {
		return errors.New("Transfer invalid for transfer tx")
	}

	// Find "first" contract. The "first" contract of a transfer is the one responsible for
	//   creating the initial settlement data and passing it to the next contract if there
	//   are more than one.
	firstContractIndex := uint16(0)
	for _, asset := range transfer.Assets {
		if asset.ContractIndex != uint16(0xffff) {
			break
		}
		// Asset transfer doesn't have a contract (probably BSV transfer).
		firstContractIndex++
	}

	if int(transfer.Assets[firstContractIndex].ContractIndex) >= len(transferTx.Outputs) {
		logger.Warn(ctx, "%s : Transfer contract index out of range : %s", v.TraceID, rk.Address.String())
		return errors.New("Transfer contract index out of range")
	}

	// Bitcoin balance of first contract
	contractBalance := uint64(transferTx.Outputs[transfer.Assets[firstContractIndex].ContractIndex].Value)

	// Build settle tx
	settleTx, err := buildSettlementTx(ctx, m.MasterDB, m.Config, transferTx, transfer, settlementRequest, contractBalance, rk)
	if err != nil {
		return errors.Wrap(err, "Failed to build settle tx")
	}

	// Serialize settlement data into OP_RETURN output as a placeholder to be updated by addSettlementData.
	var script []byte
	script, err = protocol.Serialize(settlement, m.Config.IsTest)
	if err != nil {
		logger.Warn(ctx, "%s : Failed to serialize settlement : %s", v.TraceID, err)
		return err
	}
	err = settleTx.AddOutput(script, 0, false, false)
	if err != nil {
		return err
	}

	contractPKH := protocol.PublicKeyHashFromBytes(rk.Address.ScriptAddress())
	ct, err := contract.Retrieve(ctx, m.MasterDB, contractPKH)
	if err != nil {
		return errors.Wrap(err, "Failed to retrieve contract")
	}

	if !ct.MovedTo.IsZero() {
		logger.Warn(ctx, "%s : Contract address changed : %s", v.TraceID, ct.MovedTo.String())
		return m.respondTransferMessageReject(ctx, w, itx, transferTx, transfer, rk, protocol.RejectContractMoved)
	}

	// Add this contract's data to the settlement op return data
	err = addSettlementData(ctx, m.MasterDB, m.Config, rk, transferTx, transfer, settleTx, settlement, m.Headers)
	if err != nil {
		reject, ok := err.(rejectError)
		if ok {
			logger.Warn(ctx, "%s : Rejecting Transfer : %s", v.TraceID, err)
			return m.respondTransferMessageReject(ctx, w, itx, transferTx, transfer, rk, reject.code)
		} else {
			return errors.Wrap(err, "Failed to add settlement data")
		}
	}

	// Check if settlement data is complete. No other contracts involved
	if settlementIsComplete(ctx, transfer, settlement) {
		// Sign this contracts input of the settle tx.
		signed := false
		for i, _ := range settleTx.Inputs {
			err = settleTx.SignInput(i, rk.PrivateKey)
			if txbuilder.IsErrorCode(err, txbuilder.ErrorCodeWrongPrivateKey) {
				continue
			}
			if err != nil {
				return err
			}
			logger.Verbose(ctx, "%s : Signed settlement input %d", v.TraceID, i)
			signed = true
		}

		if !signed {
			return errors.New("Failed to find input to sign")
		}

		// This shouldn't happen because we recieved this from another contract and they couldn't
		//   have signed it yet since it was incomplete.
		if settleTx.AllInputsAreSigned() {
			// Remove tracer for this request.
			boomerangIndex := findBoomerangIndex(transferTx, transfer, rk.Address)
			if boomerangIndex != 0xffffffff {
				outpoint := wire.OutPoint{Hash: transferTx.Hash, Index: boomerangIndex}
				m.Tracer.Remove(ctx, &outpoint)
			}

			logger.Info(ctx, "%s : Broadcasting settlement tx", v.TraceID)
			// Send complete settlement tx as response
			return node.Respond(ctx, w, settleTx.MsgTx)
		}

		// Send back to previous contract via a M1 - 1002 Signature Request
		return sendToPreviousSettlementContract(ctx, m.Config, w, rk, itx, settleTx)
	}

	// Save tx
	if err := transactions.AddTx(ctx, m.MasterDB, transferTx); err != nil {
		return errors.Wrap(err, "Failed to save tx")
	}

	// Send to next contract
	return sendToNextSettlementContract(ctx, w, rk, itx, transferTx, transfer, settleTx, settlement, settlementRequest, m.Tracer)
}

// processSigRequest handles an incoming Message SignatureRequest payload.
func (m *Message) processSigRequest(ctx context.Context, w *node.ResponseWriter,
	itx *inspector.Transaction, sigRequest *protocol.SignatureRequest, rk *wallet.RootKey) error {
	ctx, span := trace.StartSpan(ctx, "handlers.Message.processSigRequest")
	defer span.End()

	var tx wire.MsgTx
	buf := bytes.NewBuffer(sigRequest.Payload)
	err := tx.Deserialize(buf)
	if err != nil {
		return errors.Wrap(err, "Failed to deserialize sig request payload tx")
	}

	v := ctx.Value(node.KeyValues).(*node.Values)

	// Find OP_RETURN
	for _, output := range tx.TxOut {
		opReturn, err := protocol.Deserialize(output.PkScript, m.Config.IsTest)
		if err == nil {
			switch msg := opReturn.(type) {
			case *protocol.Settlement:
				logger.Verbose(ctx, "%s : Processing Settlement Signature Request", v.TraceID)
				return m.processSigRequestSettlement(ctx, w, itx, rk, sigRequest, &tx, msg)
			default:
				return fmt.Errorf("Unsupported signature request tx payload type : %s", opReturn.Type())
			}
		}
	}

	return fmt.Errorf("Tokenized OP_RETURN not found in Sig Request Tx : %s", tx.TxHash())
}

// processSigRequestSettlement handles an incoming Message SignatureRequest payload containing a
//   Settlement tx.
func (m *Message) processSigRequestSettlement(ctx context.Context, w *node.ResponseWriter,
	itx *inspector.Transaction, rk *wallet.RootKey, sigRequest *protocol.SignatureRequest,
	settleWireTx *wire.MsgTx, settlement *protocol.Settlement) error {
	// Get transfer tx
	transferTx, err := transactions.GetTx(ctx, m.MasterDB, &settleWireTx.TxIn[0].PreviousOutPoint.Hash, &m.Config.ChainParams, m.Config.IsTest)
	if err != nil {
		return errors.New("Failed to get transfer tx")
	}

	// Get transfer from tx
	transferMsg, ok := transferTx.MsgProto.(*protocol.Transfer)
	if !ok {
		return errors.New("Transfer invalid for transfer tx")
	}

	v := ctx.Value(node.KeyValues).(*node.Values)

	contractPKH := protocol.PublicKeyHashFromBytes(rk.Address.ScriptAddress())
	ct, err := contract.Retrieve(ctx, m.MasterDB, contractPKH)
	if err != nil {
		return errors.Wrap(err, "Failed to retrieve contract")
	}

	if !ct.MovedTo.IsZero() {
		logger.Warn(ctx, "%s : Contract address changed : %s", v.TraceID, ct.MovedTo.String())
		return m.respondTransferMessageReject(ctx, w, itx, transferTx, transferMsg, rk, protocol.RejectContractMoved)
	}

	// Verify all the data for this contract is correct.
	err = verifySettlement(ctx, w.Config, m.MasterDB, rk, transferTx, transferMsg, settleWireTx, settlement, m.Headers)
	if err != nil {
		reject, ok := err.(rejectError)
		if ok {
			logger.Warn(ctx, "%s : Rejecting Transfer : %s", v.TraceID, err)
			return m.respondTransferMessageReject(ctx, w, itx, transferTx, transferMsg, rk, reject.code)
		} else {
			return errors.Wrap(err, "Failed to verify settlement data")
		}
	}

	// Convert settle tx to a txbuilder tx
	var settleTx *txbuilder.Tx
	settleTx, err = txbuilder.NewTxFromWire(rk.Address.ScriptAddress(), m.Config.DustLimit, m.Config.FeeRate,
		settleWireTx, []*wire.MsgTx{transferTx.MsgTx})
	if err != nil {
		return errors.Wrap(err, "Failed to compose settle tx")
	}

	// Sign this contracts input of the settle tx.
	signed := false
	for i, _ := range settleTx.Inputs {
		err = settleTx.SignInput(i, rk.PrivateKey)
		if txbuilder.IsErrorCode(err, txbuilder.ErrorCodeWrongPrivateKey) {
			continue
		}
		if err != nil {
			return err
		}
		logger.Verbose(ctx, "%s : Signed settlement input %d", v.TraceID, i)
		signed = true
	}

	if !signed {
		return errors.New("Failed to find input to sign")
	}

	// This shouldn't happen because we recieved this from another contract and they couldn't
	//   have signed it yet since it was incomplete.
	if settleTx.AllInputsAreSigned() {
		// Remove pending transfer
		contractPKH := protocol.PublicKeyHashFromBytes(rk.Address.ScriptAddress())
		if err := transfer.Remove(ctx, m.MasterDB, contractPKH, protocol.TxIdFromBytes(transferTx.Hash[:])); err != nil {
			return errors.Wrap(err, "Failed to save pending transfer")
		}

		// Cancel transfer timeout
		err := m.Scheduler.CancelJob(ctx, listeners.NewTransferTimeout(nil, transferTx, protocol.NewTimestamp(0)))
		if err != nil {
			if err == scheduler.NotFound {
				logger.Warn(ctx, "%s : Transfer timeout job not found to cancel", v.TraceID)
			} else {
				return errors.Wrap(err, "Failed to cancel transfer timeout")
			}
		}

		logger.Info(ctx, "%s : Broadcasting settlement tx", v.TraceID)
		// Send complete settlement tx as response
		return node.Respond(ctx, w, settleTx.MsgTx)
	}

	// Send back to previous contract via a M1 - 1002 Signature Request
	return sendToPreviousSettlementContract(ctx, m.Config, w, rk, itx, settleTx)
}

// sendToPreviousSettlementContract sends the completed settlement tx to the previous contract involved so it can sign it.
func sendToPreviousSettlementContract(ctx context.Context, config *node.Config, w *node.ResponseWriter,
	rk *wallet.RootKey, itx *inspector.Transaction, settleTx *txbuilder.Tx) error {
	ctx, span := trace.StartSpan(ctx, "handlers.Message.sendToPreviousSettlementContract")
	defer span.End()

	// Find previous input that still needs a signature
	inputIndex := 0xffffffff
	for i, _ := range settleTx.MsgTx.TxIn {
		if !settleTx.InputIsSigned(i) {
			inputIndex = i
		}
	}

	// This only happens if this function was called in error with a completed tx.
	if inputIndex == 0xffffffff {
		return errors.New("Could not find input that needs signature")
	}

	pkh, err := settleTx.InputPKH(inputIndex)
	if err != nil {
		return err
	}

	address, err := btcutil.NewAddressPubKeyHash(pkh, &config.ChainParams)
	if err != nil {
		return err
	}

	v := ctx.Value(node.KeyValues).(*node.Values)

	logger.Info(ctx, "%s : Sending settlement SignatureRequest to %x", v.TraceID, address.ScriptAddress())

	// Add output to previous contract.
	// Mark as change so it gets everything except the tx fee.
	err = w.AddChangeOutput(ctx, address)
	if err != nil {
		return err
	}

	// Serialize settlement data for Message OP_RETURN.
	var buf bytes.Buffer
	err = settleTx.MsgTx.Serialize(&buf)
	if err != nil {
		return err
	}

	messagePayload := protocol.SignatureRequest{
		Version:   0,
		Timestamp: v.Now,
		Payload:   buf.Bytes(),
	}

	// Setup Message
	var data []byte
	data, err = messagePayload.Serialize()
	if err != nil {
		return err
	}
	message := protocol.Message{
		AddressIndexes: []uint16{0}, // First output is receiver of message
		MessageType:    messagePayload.Type(),
		MessagePayload: data,
	}

	return node.RespondSuccess(ctx, w, itx, rk, &message)
}

// verifySettlement verifies that all settlement data related to this contract and bitcoin transfers are correct.
func verifySettlement(ctx context.Context, config *node.Config, masterDB *db.DB, rk *wallet.RootKey,
	transferTx *inspector.Transaction, transfer *protocol.Transfer, settleTx *wire.MsgTx,
	settlement *protocol.Settlement, headers BitcoinHeaders) error {
	ctx, span := trace.StartSpan(ctx, "handlers.Transfer.verifySettlement")
	defer span.End()

	v := ctx.Value(node.KeyValues).(*node.Values)

	// Generate public key hashes for all the outputs
	settleOutputPKHs := make([]*protocol.PublicKeyHash, 0, len(settleTx.TxOut))
	for _, output := range settleTx.TxOut {
		pkh, err := txbuilder.PubKeyHashFromP2PKH(output.PkScript)
		if err == nil {
			settleOutputPKHs = append(settleOutputPKHs, protocol.PublicKeyHashFromBytes(pkh))
		} else {
			settleOutputPKHs = append(settleOutputPKHs, nil)
		}
	}

	// Generate public key hashes for all the inputs
	hash256 := sha256.New()
	hash160 := ripemd160.New()
	settleInputAddresses := make([]protocol.PublicKeyHash, 0, len(settleTx.TxIn))
	for _, input := range settleTx.TxIn {
		pushes, err := txscript.PushedData(input.SignatureScript)
		if err != nil {
			settleInputAddresses = append(settleInputAddresses, protocol.PublicKeyHash{})
			continue
		}
		if len(pushes) != 2 {
			settleInputAddresses = append(settleInputAddresses, protocol.PublicKeyHash{})
			continue
		}

		// Calculate RIPEMD160(SHA256(PublicKey))
		hash256.Reset()
		hash256.Write(pushes[1])
		hash160.Reset()
		hash160.Write(hash256.Sum(nil))
		settleInputAddresses = append(settleInputAddresses, *protocol.PublicKeyHashFromBytes(hash160.Sum(nil)))
	}

	contractPKH := protocol.PublicKeyHashFromBytes(rk.Address.ScriptAddress())
	ct, err := contract.Retrieve(ctx, masterDB, contractPKH)
	if err != nil {
		return errors.Wrap(err, "Failed to retrieve contract")
	}

	for assetOffset, assetTransfer := range transfer.Assets {
		assetIsBitcoin := assetTransfer.AssetType == "CUR" && assetTransfer.AssetCode.IsZero()

		var as *state.Asset
		if !assetIsBitcoin {
			if len(settleTx.TxOut) <= int(assetTransfer.ContractIndex) {
				return fmt.Errorf("Contract index out of range for asset %d", assetOffset)
			}

			contractOutputPKH := settleOutputPKHs[assetTransfer.ContractIndex]
			if contractOutputPKH != nil && !bytes.Equal(contractOutputPKH.Bytes(), contractPKH.Bytes()) {
				continue // This asset is not for this contract.
			}
			if ct.FreezePeriod.Nano() > v.Now.Nano() {
				return rejectError{code: protocol.RejectContractFrozen}
			}

			// Locate Asset
			as, err = asset.Retrieve(ctx, masterDB, contractPKH, &assetTransfer.AssetCode)
			if err != nil {
				return fmt.Errorf("Asset ID not found : %s %s : %s", contractPKH, assetTransfer.AssetCode, err)
			}
			if as.FreezePeriod.Nano() > v.Now.Nano() {
				return rejectError{code: protocol.RejectAssetFrozen}
			}
			if !as.TransfersPermitted {
				return rejectError{code: protocol.RejectAssetNotPermitted}
			}
		}

		// Find settlement for asset.
		var assetSettlement *protocol.AssetSettlement
		for i, asset := range settlement.Assets {
			if asset.AssetType == assetTransfer.AssetType &&
				bytes.Equal(asset.AssetCode.Bytes(), assetTransfer.AssetCode.Bytes()) {
				assetSettlement = &settlement.Assets[i]
				break
			}
		}

		if assetSettlement == nil {
			return fmt.Errorf("Asset settlement not found during verify")
		}

		sendBalance := uint64(0)
		settlementQuantities := make([]*uint64, len(settleTx.TxOut))

		// Process senders
		// assetTransfer.AssetSenders []QuantityIndex {Index uint16, Quantity uint64}
		for senderOffset, sender := range assetTransfer.AssetSenders {
			// Get sender address from transfer inputs[sender.Index]
			if int(sender.Index) >= len(transferTx.Inputs) {
				return fmt.Errorf("Sender input index out of range for asset %d sender %d : %d/%d",
					assetOffset, senderOffset, sender.Index, len(transferTx.Inputs))
			}

			inputPKH := transferTx.Inputs[sender.Index].Address.ScriptAddress()

			// Find output in settle tx
			settleOutputIndex := uint16(0xffff)
			for i, outputPKH := range settleOutputPKHs {
				if outputPKH != nil && bytes.Equal(outputPKH.Bytes(), inputPKH) {
					settleOutputIndex = uint16(i)
					break
				}
			}

			if settleOutputIndex == uint16(0xffff) {
				return fmt.Errorf("Sender output not found in settle tx for asset %d sender %d : %d/%d",
					assetOffset, senderOffset, sender.Index, len(transferTx.Outputs))
			}

			// Check sender's available unfrozen balance
			inputAddress := protocol.PublicKeyHashFromBytes(inputPKH)
			if !assetIsBitcoin && !asset.CheckBalanceFrozen(ctx, as, inputAddress, sender.Quantity, v.Now) {
				logger.Warn(ctx, "%s : Frozen funds: contract=%s asset=%s party=%s", v.TraceID,
					contractPKH, assetTransfer.AssetCode, inputAddress)
				return rejectError{code: protocol.RejectHoldingsFrozen}
			}

			if settlementQuantities[settleOutputIndex] == nil {
				// Get sender's balance
				if assetIsBitcoin {
					senderBalance := uint64(0)
					settlementQuantities[settleOutputIndex] = &senderBalance
				} else {
					senderBalance := asset.GetBalance(ctx, as, inputAddress)
					settlementQuantities[settleOutputIndex] = &senderBalance
				}
			}

			if *settlementQuantities[settleOutputIndex] < sender.Quantity {
				logger.Warn(ctx, "%s : Insufficient funds: contract=%s asset=%s party=%s", v.TraceID,
					contractPKH, assetTransfer.AssetCode, inputAddress)
				return rejectError{code: protocol.RejectInsufficientQuantity}
			}

			*settlementQuantities[settleOutputIndex] -= sender.Quantity

			// Update total send balance
			sendBalance += sender.Quantity
		}

		// Process receivers
		for receiverOffset, receiver := range assetTransfer.AssetReceivers {
			// Get receiver address from outputs[receiver.Index]
			if int(receiver.Index) >= len(transferTx.Outputs) {
				return fmt.Errorf("Receiver output index out of range for asset %d sender %d : %d/%d",
					assetOffset, receiverOffset, receiver.Index, len(transferTx.Outputs))
			}

			receiverPKH := protocol.PublicKeyHashFromBytes(transferTx.Outputs[receiver.Index].Address.ScriptAddress())

			// Find output in settle tx
			settleOutputIndex := uint16(0xffff)
			for i, outputPKH := range settleOutputPKHs {
				if outputPKH != nil && bytes.Equal(outputPKH.Bytes(), receiverPKH.Bytes()) {
					settleOutputIndex = uint16(i)
					break
				}
			}

			if settleOutputIndex == uint16(0xffff) {
				return fmt.Errorf("Receiver output not found in settle tx for asset %d receiver %d : %d/%d",
					assetOffset, receiverOffset, receiver.Index, len(transferTx.Outputs))
			}

			// Validate Oracle Signature
			if err := validateOracle(ctx, contractPKH, ct, &assetTransfer.AssetCode, receiverPKH, &receiver,
				headers); err != nil {
				return rejectError{code: protocol.RejectInvalidSignature, text: err.Error()}
			}

			if settlementQuantities[settleOutputIndex] == nil {
				// Get receiver's balance
				if assetIsBitcoin {
					receiverBalance := uint64(0)
					settlementQuantities[settleOutputIndex] = &receiverBalance
				} else {
					receiverBalance := asset.GetBalance(ctx, as, receiverPKH)
					settlementQuantities[settleOutputIndex] = &receiverBalance
				}
			}

			*settlementQuantities[settleOutputIndex] += receiver.Quantity

			// Update asset balance
			if receiver.Quantity > sendBalance {
				return fmt.Errorf("Receiving more tokens than sending for asset %d", assetOffset)
			}
			sendBalance -= receiver.Quantity
		}

		if sendBalance != 0 {
			return fmt.Errorf("Not sending all input tokens for asset %d : %d remaining", assetOffset, sendBalance)
		}

		// Check ending balances
		for index, quantity := range settlementQuantities {
			if quantity != nil {
				found := false
				for _, settlementQuantity := range assetSettlement.Settlements {
					if index == int(settlementQuantity.Index) {
						if *quantity != settlementQuantity.Quantity {
							logger.Warn(ctx, "%s : Incorrect settlment quantity for output %d : %d != %d : %x",
								v.TraceID, index, *quantity, settlementQuantity.Quantity, assetTransfer.AssetCode)
							return fmt.Errorf("Asset settlement quantity wrong")
						}
						found = true
						break
					}
				}

				if !found {
					logger.Warn(ctx, "%s : missing settlment for output %d : %x",
						v.TraceID, index, assetTransfer.AssetCode)
					return fmt.Errorf("Asset settlement missing")
				}
			}
		}
	}

	// Verify contract fee
	if ct.ContractFee > 0 {
		found := false
		for i, outputPKH := range settleOutputPKHs {
			if outputPKH != nil && bytes.Equal(outputPKH.Bytes(), config.FeePKH.Bytes()) {
				if uint64(settleTx.TxOut[i].Value) < ct.ContractFee {
					return fmt.Errorf("Contract fee too low")
				}
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("Contract fee missing")
		}
	}

	return nil
}

// respondTransferMessageReject responds to an M1 Offer or SigRequest with a rejection message.
// If this is the first contract, it will send a full refund/reject to all parties involved.
// If this is not the first contract, it will send a reject message to the first contract so that
//   it can send the refund/reject to everyone.
func (m *Message) respondTransferMessageReject(ctx context.Context, w *node.ResponseWriter,
	messageTx *inspector.Transaction, transferTx *inspector.Transaction, transfer *protocol.Transfer,
	rk *wallet.RootKey, code uint8) error {

	// Determine if first contract
	first := firstContractOutputIndex(transfer.Assets, transferTx)
	if first == 0xffff {
		return errors.New("First contract output index not found")
	}

	if !bytes.Equal(transferTx.Outputs[first].Address.ScriptAddress(), rk.Address.ScriptAddress()) {
		// This is not the first contract. Send reject to only the first contract.
		w.AddRejectValue(ctx, transferTx.Outputs[first].Address, 0)
		return node.RespondReject(ctx, w, messageTx, rk, code)
	}

	// Determine UTXOs from transfer tx to fund the reject response.
	utxos, err := transferTx.UTXOs().ForAddress(rk.Address)
	if err != nil {
		return errors.Wrap(err, "Transfer UTXOs not found")
	}

	// Remove utxo spent by boomerang
	boomerangIndex := findBoomerangIndex(transferTx, transfer, rk.Address)
	if boomerangIndex == 0xffffffff {
		return errors.New("Boomerang output index not found")
	}

	if bytes.Equal(transferTx.Outputs[boomerangIndex].Address.ScriptAddress(), rk.Address.ScriptAddress()) {
		found := false
		for i, utxo := range utxos {
			if utxo.Index == boomerangIndex {
				found = true
				utxos = append(utxos[:i], utxos[i+1:]...) // Remove
				break
			}
		}

		if !found {
			return errors.New("Boomerang output not found")
		}
	}

	// Add utxo from message tx
	messageUTXOs, err := messageTx.UTXOs().ForAddress(rk.Address)
	if err != nil {
		return errors.Wrap(err, "Message UTXOs not found")
	}

	utxos = append(utxos, messageUTXOs...)

	balance := uint64(0)
	for _, utxo := range utxos {
		balance += uint64(utxo.Value)
	}

	w.SetRejectUTXOs(ctx, utxos)

	// Add refund amounts for all bitcoin senders
	refundBalance := uint64(0)
	for _, assetTransfer := range transfer.Assets {
		if assetTransfer.AssetType == "CUR" && assetTransfer.AssetCode.IsZero() {
			// Process bitcoin senders refunds
			for _, sender := range assetTransfer.AssetSenders {
				if int(sender.Index) >= len(transferTx.Inputs) {
					continue
				}

				w.AddRejectValue(ctx, transferTx.Inputs[sender.Index].Address, sender.Quantity)
				refundBalance += sender.Quantity
			}
		} else {
			// Add all other senders to be notified
			for _, sender := range assetTransfer.AssetSenders {
				if int(sender.Index) >= len(transferTx.Inputs) {
					continue
				}

				w.AddRejectValue(ctx, transferTx.Inputs[sender.Index].Address, 0)
			}
		}
	}

	if refundBalance > balance {
		contractPKH := protocol.PublicKeyHashFromBytes(rk.Address.ScriptAddress())
		ct, err := contract.Retrieve(ctx, m.MasterDB, contractPKH)
		if err != nil {
			return errors.Wrap(err, "Failed to retrieve contract")
		}

		// Funding not enough to refund everyone, so don't refund to anyone. Send it to the issuer to hold.
		issuerAddress, err := btcutil.NewAddressPubKeyHash(ct.IssuerPKH.Bytes(), &m.Config.ChainParams)
		w.ClearRejectOutputValues(issuerAddress)
	}

	return node.RespondReject(ctx, w, transferTx, rk, code)
}

// refundTransferFromReject responds to an M2 Reject, from another contract involved in a multi-contract
//   transfer with a tx refunding any bitcoin sent to the contract that was requested to be
//   transferred.
func refundTransferFromReject(ctx context.Context, masterDB *db.DB, sch *scheduler.Scheduler, config *node.Config,
	w *node.ResponseWriter, rejectionTx *inspector.Transaction, rejection *protocol.Rejection,
	transferTx *inspector.Transaction, transferMsg *protocol.Transfer, rk *wallet.RootKey) error {

	// Remove pending transfer
	contractPKH := protocol.PublicKeyHashFromBytes(rk.Address.ScriptAddress())
	if err := transfer.Remove(ctx, masterDB, contractPKH, protocol.TxIdFromBytes(transferTx.Hash[:])); err != nil {
		return errors.Wrap(err, "Failed to save pending transfer")
	}

	// Cancel transfer timeout
	v := ctx.Value(node.KeyValues).(*node.Values)
	err := sch.CancelJob(ctx, listeners.NewTransferTimeout(nil, transferTx, protocol.NewTimestamp(0)))
	if err != nil {
		if err == scheduler.NotFound {
			logger.Warn(ctx, "%s : Transfer timeout job not found to cancel", v.TraceID)
		} else {
			return errors.Wrap(err, "Failed to cancel transfer timeout")
		}
	}

	// Find first contract index.
	first := firstContractOutputIndex(transferMsg.Assets, transferTx)
	if first == 0xffff {
		return errors.New("First contract output index not found")
	}

	// Determine if this contract is the first contract an needs to send a refund.
	if !bytes.Equal(transferTx.Outputs[first].Address.ScriptAddress(), rk.Address.ScriptAddress()) {
		return nil
	}

	// Determine UTXOs from transfer tx to fund the reject response.
	utxos, err := transferTx.UTXOs().ForAddress(rk.Address)
	if err != nil {
		return errors.Wrap(err, "Transfer UTXOs not found")
	}

	// Remove utxo spent by boomerang
	boomerangIndex := findBoomerangIndex(transferTx, transferMsg, rk.Address)
	if boomerangIndex == 0xffffffff {
		return errors.New("Boomerang output index not found")
	}

	if bytes.Equal(transferTx.Outputs[boomerangIndex].Address.ScriptAddress(), rk.Address.ScriptAddress()) {
		found := false
		for i, utxo := range utxos {
			if utxo.Index == boomerangIndex {
				found = true
				utxos = append(utxos[:i], utxos[i+1:]...) // Remove
				break
			}
		}

		if !found {
			return errors.New("Boomerang output not found")
		}
	}

	// Add utxo from message tx
	messageUTXOs, err := rejectionTx.UTXOs().ForAddress(rk.Address)
	if err != nil {
		return errors.Wrap(err, "Message UTXOs not found")
	}

	utxos = append(utxos, messageUTXOs...)

	balance := uint64(0)
	for _, utxo := range utxos {
		balance += uint64(utxo.Value)
	}

	w.SetRejectUTXOs(ctx, utxos)

	// Add refund amounts for all bitcoin senders
	refundBalance := uint64(0)
	for _, assetTransfer := range transferMsg.Assets {
		if assetTransfer.AssetType == "CUR" && assetTransfer.AssetCode.IsZero() {
			// Process bitcoin senders refunds
			for _, sender := range assetTransfer.AssetSenders {
				if int(sender.Index) >= len(transferTx.Inputs) {
					continue
				}

				w.AddRejectValue(ctx, transferTx.Inputs[sender.Index].Address, sender.Quantity)
				refundBalance += sender.Quantity
			}
		} else {
			// Add all other senders to be notified
			for _, sender := range assetTransfer.AssetSenders {
				if int(sender.Index) >= len(transferTx.Inputs) {
					continue
				}

				w.AddRejectValue(ctx, transferTx.Inputs[sender.Index].Address, 0)
			}
		}
	}

	if refundBalance > balance {
		contractPKH := protocol.PublicKeyHashFromBytes(rk.Address.ScriptAddress())
		ct, err := contract.Retrieve(ctx, masterDB, contractPKH)
		if err != nil {
			return errors.Wrap(err, "Failed to retrieve contract")
		}

		// Funding not enough to refund everyone, so don't refund to anyone.
		issuerAddress, err := btcutil.NewAddressPubKeyHash(ct.IssuerPKH.Bytes(), &config.ChainParams)
		w.ClearRejectOutputValues(issuerAddress)
	}

	// Set rejection address from previous rejection
	if int(rejection.RejectAddressIndex) < len(rejectionTx.Outputs) {
		w.RejectPKH = protocol.PublicKeyHashFromBytes(rejectionTx.Outputs[rejection.RejectAddressIndex].Address.ScriptAddress())
	}

	return node.RespondReject(ctx, w, transferTx, rk, rejection.RejectionCode)
}
