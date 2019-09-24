package txbuilder

import (
	"fmt"

	"github.com/tokenized/smart-contract/pkg/bitcoin"
	"github.com/tokenized/smart-contract/pkg/wire"

	"github.com/pkg/errors"
)

// InputSupplement contains data required to sign an input that is not already in the wire.MsgTx.
type InputSupplement struct {
	LockingScript []byte `json:"locking_script"`
	Value         uint64 `json:"value"`

	// Optional identifier for external use to track the key needed to sign the input.
	KeyID string `json:"key_id,omitempty"`
}

// InputAddress returns the address that is paying to the input.
func (tx *TxBuilder) InputAddress(index int) (bitcoin.RawAddress, error) {
	if index >= len(tx.Inputs) {
		return bitcoin.RawAddress{}, errors.New("Input index out of range")
	}
	return bitcoin.RawAddressFromLockingScript(tx.Inputs[index].LockingScript)
}

// AddInput adds an input to TxBuilder.
func (tx *TxBuilder) AddInputUTXO(utxo bitcoin.UTXO) error {
	input := InputSupplement{
		LockingScript: utxo.LockingScript,
		Value:         utxo.Value,
		KeyID:         utxo.KeyID,
	}
	tx.Inputs = append(tx.Inputs, &input)

	txin := wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: utxo.Hash, Index: utxo.Index},
		Sequence:         wire.MaxTxInSequenceNum,
	}
	tx.MsgTx.AddTxIn(&txin)
	return nil
}

// AddInput adds an input to TxBuilder.
//   outpoint reference the output being spent.
//   lockScript is the script from the output being spent.
//   value is the number of satoshis from the output being spent.
func (tx *TxBuilder) AddInput(outpoint wire.OutPoint, lockScript []byte, value uint64) error {
	input := InputSupplement{
		LockingScript: lockScript,
		Value:         value,
	}
	tx.Inputs = append(tx.Inputs, &input)

	txin := wire.TxIn{
		PreviousOutPoint: outpoint,
		Sequence:         wire.MaxTxInSequenceNum,
	}
	tx.MsgTx.AddTxIn(&txin)
	return nil
}

// SetSendMax sets the SendMax flag so AddFunding will add all UTXOs as inputs, even if they are not
//   needed to fund outputs.
func (tx *TxBuilder) SetSendMax() {
	tx.SendMax = true
}

// AddFunding adds inputs spending the specified UTXOs until the transaction has enough funding to
//   cover the fees and outputs.
// If SendMax is set then all UTXOs are added as inputs.
func (tx *TxBuilder) AddFunding(utxos []bitcoin.UTXO) error {

	inputValue := tx.InputValue()
	outputValue := tx.OutputValue(true)
	feeValue := tx.Fee()
	estFeeValue := tx.EstimatedFee()
	estFeeLow := uint64(float32(estFeeValue) * 0.95)

	if !tx.SendMax && feeValue > estFeeLow {
		return nil // Already funded
	}

	// Find change output
	changeOutputIndex := 0xffffffff
	for i, output := range tx.Outputs {
		if output.IsRemainder {
			changeOutputIndex = i
			break
		}
	}

	// Calculate additional funding needed. Include cost of first added input.
	// TODO Add support for input scripts other than P2PKH.
	funding := estFeeValue + outputValue - inputValue
	estInputFee := uint64(float32(EstimatedP2PKHInputSize) * tx.FeeRate)
	estOutputFee := uint64(float32(P2PKHOutputSize) * tx.FeeRate)
	funding += estInputFee
	if changeOutputIndex == 0xffffffff {
		funding += estOutputFee // Change output
	}

	var err error
	for _, utxo := range utxos {
		err = tx.AddInputUTXO(utxo)
		if err != nil {
			return errors.Wrap(err, "adding input")
		}

		if tx.SendMax {
			continue
		}

		if funding > utxo.Value {
			funding -= utxo.Value // More UTXOs required
		} else {
			// Funding complete
			change := utxo.Value - funding
			if changeOutputIndex == 0xffffffff {
				if change > tx.DustLimit {
					if tx.ChangeAddress.IsEmpty() {
						return errors.New("Change address needed")
					}
					err = tx.AddPaymentOutput(tx.ChangeAddress, change, true)
					if err != nil {
						return errors.Wrap(err, "adding change")
					}
					tx.Outputs[len(tx.Outputs)-1].KeyID = tx.ChangeKeyID
				}
			} else {
				tx.MsgTx.TxOut[changeOutputIndex].Value += change
			}
			funding = 0
			return nil
		}

		// Add cost of next input
		funding += uint64(float32(EstimatedP2PKHInputSize) * tx.FeeRate)
	}

	if tx.SendMax {
		return tx.CalculateFee()
	} else {
		available := uint64(0)
		for _, utxo := range utxos {
			available += utxo.Value
		}
		return fmt.Errorf("insufficient funding %d/%d", available, tx.EstimatedFee())
	}

	return nil
}
