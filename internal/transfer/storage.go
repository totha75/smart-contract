package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tokenized/smart-contract/internal/platform/db"
	"github.com/tokenized/smart-contract/internal/platform/state"
	"github.com/tokenized/smart-contract/pkg/bitcoin"
	"github.com/tokenized/specification/dist/golang/protocol"
)

const storageKey = "contracts"
const storageSubKey = "transfers"

var (
	// ErrNotFound abstracts the standard not found error.
	ErrNotFound = errors.New("Pending transfer not found")
)

// Put a single pending transfer in storage
func Save(ctx context.Context, dbConn *db.DB, contractAddress bitcoin.RawAddress, t *state.PendingTransfer) error {
	key := buildStoragePath(contractAddress, t.TransferTxId)

	// Save the contract
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}

	return dbConn.Put(ctx, key, data)
}

// Fetch a single pending transfer from storage
func Fetch(ctx context.Context, dbConn *db.DB, contractAddress bitcoin.RawAddress, transferTxId *protocol.TxId) (*state.PendingTransfer, error) {
	key := buildStoragePath(contractAddress, transferTxId)

	data, err := dbConn.Fetch(ctx, key)
	if err != nil {
		if err == db.ErrNotFound {
			err = ErrNotFound
		}

		return nil, err
	}

	// Prepare the pending transfer object
	result := state.PendingTransfer{}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}

	return &result, nil
}

func Remove(ctx context.Context, dbConn *db.DB, contractAddress bitcoin.RawAddress, transferTxId *protocol.TxId) error {
	err := dbConn.Remove(ctx, buildStoragePath(contractAddress, transferTxId))
	if err != nil {
		if err == db.ErrNotFound {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// List all pending transfer for a specified contract.
func List(ctx context.Context, dbConn *db.DB, contractAddress bitcoin.RawAddress) ([]*state.PendingTransfer, error) {

	// TODO: This should probably use dbConn.List for greater efficiency
	data, err := dbConn.Search(ctx, fmt.Sprintf("%s/%x/%s", storageKey, contractAddress.Bytes(), storageSubKey))
	if err != nil {
		return nil, err
	}

	result := make([]*state.PendingTransfer, 0, len(data))
	for _, b := range data {
		pendingTransfer := state.PendingTransfer{}

		if err := json.Unmarshal(b, &pendingTransfer); err != nil {
			return nil, err
		}

		result = append(result, &pendingTransfer)
	}

	return result, nil
}

// Returns the storage path prefix for a given identifier.
func buildStoragePath(contractAddress bitcoin.RawAddress, txid *protocol.TxId) string {
	return fmt.Sprintf("%s/%x/%s/%s", storageKey, contractAddress.Bytes(), storageSubKey, txid.String())
}
