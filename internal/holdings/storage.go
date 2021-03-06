package holdings

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/tokenized/smart-contract/internal/platform/db"
	"github.com/tokenized/smart-contract/internal/platform/state"
	"github.com/tokenized/smart-contract/pkg/bitcoin"

	"github.com/tokenized/specification/dist/golang/protocol"

	"github.com/pkg/errors"
)

var (
	ErrNotInCache = errors.New("Not in cache")
)

// Options
//   Periodically write holdings that haven't been written for x seconds
//   Build deltas and only write deltas
//     holding statuses are fixed size
//

const storageKey = "contracts"
const storageSubKey = "holdings"

type cacheUpdate struct {
	h        *state.Holding
	modified bool // true when modified since last write to storage.
	lock     sync.Mutex
}

var cache map[bitcoin.Hash20]map[protocol.AssetCode]map[bitcoin.Hash20]*cacheUpdate
var cacheLock sync.Mutex

// Save puts a single holding in cache. A CacheItem is returned and should be put in a CacheChannel
//   to be written to storage asynchronously, or be synchronously written to storage by immediately
//   calling Write.
func Save(ctx context.Context, dbConn *db.DB, contractAddress bitcoin.RawAddress,
	assetCode *protocol.AssetCode, h *state.Holding) (*CacheItem, error) {

	cacheLock.Lock()
	defer cacheLock.Unlock()

	if cache == nil {
		cache = make(map[bitcoin.Hash20]map[protocol.AssetCode]map[bitcoin.Hash20]*cacheUpdate)
	}
	contractHash, err := contractAddress.Hash()
	if err != nil {
		return nil, err
	}
	contract, exists := cache[*contractHash]
	if !exists {
		contract = make(map[protocol.AssetCode]map[bitcoin.Hash20]*cacheUpdate)
		cache[*contractHash] = contract
	}
	asset, exists := contract[*assetCode]
	if !exists {
		asset = make(map[bitcoin.Hash20]*cacheUpdate)
		contract[*assetCode] = asset
	}

	addressHash, err := h.Address.Hash()
	if err != nil {
		return nil, err
	}
	cu, exists := asset[*addressHash]

	if exists {
		cu.lock.Lock()
		cu.h = h
		cu.modified = true
		cu.lock.Unlock()
	} else {
		asset[*addressHash] = &cacheUpdate{h: h, modified: true}
	}

	return NewCacheItem(contractHash, assetCode, addressHash), nil
}

// List provides a list of all holdings in storage for a specified asset.
func List(ctx context.Context,
	dbConn *db.DB,
	contractAddress bitcoin.RawAddress,
	assetCode *protocol.AssetCode) ([]string, error) {

	contractHash, err := contractAddress.Hash()
	if err != nil {
		return nil, err
	}

	path := fmt.Sprintf("%s/%s/%s/%s",
		storageKey,
		contractHash.String(),
		storageSubKey,
		assetCode.String())

	return dbConn.List(ctx, path)
}

// FetchAll fetches a single holding from storage for a specified asset.
func FetchAll(ctx context.Context,
	dbConn *db.DB,
	contractAddress bitcoin.RawAddress,
	assetCode *protocol.AssetCode) ([]*state.Holding, error) {

	contractHash, err := contractAddress.Hash()
	if err != nil {
		return nil, err
	}

	path := fmt.Sprintf("%s/%s/%s/%s",
		storageKey,
		contractHash.String(),
		storageSubKey,
		assetCode.String())

	keys, err := dbConn.List(ctx, path)
	if err != nil {
		return nil, err
	}

	results := make([]*state.Holding, 0, len(keys))
	for _, key := range keys {
		b, err := dbConn.Fetch(ctx, key)
		if err != nil {
			if err == db.ErrNotFound {
				return nil, ErrNotFound
			}

			return nil, errors.Wrap(err, "Failed to fetch holding")
		}

		// Prepare the asset object
		readResult, err := deserializeHolding(bytes.NewReader(b))
		if err != nil {
			return nil, errors.Wrap(err, "Failed to deserialize holding")
		}

		results = append(results, readResult)
	}

	return results, nil
}

// Fetch fetches a single holding from storage and places it in the cache.
func Fetch(ctx context.Context, dbConn *db.DB, contractAddress bitcoin.RawAddress,
	assetCode *protocol.AssetCode, address bitcoin.RawAddress) (*state.Holding, error) {

	cacheLock.Lock()
	defer cacheLock.Unlock()

	if cache == nil {
		cache = make(map[bitcoin.Hash20]map[protocol.AssetCode]map[bitcoin.Hash20]*cacheUpdate)
	}
	contractHash, err := contractAddress.Hash()
	if err != nil {
		return nil, err
	}
	contract, exists := cache[*contractHash]
	if !exists {
		contract = make(map[protocol.AssetCode]map[bitcoin.Hash20]*cacheUpdate)
		cache[*contractHash] = contract
	}
	asset, exists := contract[*assetCode]
	if !exists {
		asset = make(map[bitcoin.Hash20]*cacheUpdate)
		contract[*assetCode] = asset
	}
	addressHash, err := address.Hash()
	if err != nil {
		return nil, err
	}
	cu, exists := asset[*addressHash]
	if exists {
		// Copy so the object in cache will not be unintentionally modified (by reference)
		// We don't want it to be modified unless Save is called.
		cu.lock.Lock()
		defer cu.lock.Unlock()
		return copyHolding(cu.h), nil
	}

	key := buildStoragePath(contractHash, assetCode, addressHash)

	b, err := dbConn.Fetch(ctx, key)
	if err != nil {
		if err == db.ErrNotFound {
			return nil, ErrNotFound
		}

		return nil, errors.Wrap(err, "Failed to fetch holding")
	}

	// Prepare the asset object
	readResult, err := deserializeHolding(bytes.NewReader(b))
	if err != nil {
		return nil, errors.Wrap(err, "Failed to deserialize holding")
	}

	asset[*addressHash] = &cacheUpdate{h: readResult, modified: false}

	return copyHolding(readResult), nil
}

// ProcessCacheItems waits for items on the cache channel and writes them to storage. It exits when
//   the channel is closed.
func ProcessCacheItems(ctx context.Context, dbConn *db.DB, ch *CacheChannel) error {
	for ci := range ch.Channel {
		if err := ci.Write(ctx, dbConn); err != nil && err != ErrNotInCache {
			return err
		}
	}

	return nil
}

func copyHolding(h *state.Holding) *state.Holding {
	result := *h
	result.HoldingStatuses = make(map[protocol.TxId]*state.HoldingStatus)
	for key, val := range h.HoldingStatuses {
		result.HoldingStatuses[key] = val
	}
	return &result
}

func Reset(ctx context.Context) {
	cacheLock.Lock()
	defer cacheLock.Unlock()

	cache = nil
}

func WriteCache(ctx context.Context, dbConn *db.DB) error {
	cacheLock.Lock()
	defer cacheLock.Unlock()

	if cache == nil {
		return nil
	}

	for contractHash, assets := range cache {
		for assetCode, assetHoldings := range assets {
			for addressHash, cu := range assetHoldings {
				cu.lock.Lock()
				if cu.modified {
					if err := write(ctx, dbConn, &contractHash, &assetCode, &addressHash, cu.h); err != nil {
						cu.lock.Unlock()
						return err
					}
					cu.modified = false
				}
				cu.lock.Unlock()
			}
		}
	}
	return nil
}

// Write updates storage for an item from the cache if it has been modified since the last write.
func WriteCacheUpdate(ctx context.Context, dbConn *db.DB, contractHash *bitcoin.Hash20,
	assetCode *protocol.AssetCode, addressHash *bitcoin.Hash20) error {

	cacheLock.Lock()
	defer cacheLock.Unlock()

	if cache == nil {
		cache = make(map[bitcoin.Hash20]map[protocol.AssetCode]map[bitcoin.Hash20]*cacheUpdate)
	}
	contract, exists := cache[*contractHash]
	if !exists {
		contract = make(map[protocol.AssetCode]map[bitcoin.Hash20]*cacheUpdate)
		cache[*contractHash] = contract
	}
	asset, exists := contract[*assetCode]
	if !exists {
		asset = make(map[bitcoin.Hash20]*cacheUpdate)
		contract[*assetCode] = asset
	}
	cu, exists := asset[*addressHash]
	if !exists {
		return ErrNotInCache
	}

	cu.lock.Lock()
	defer cu.lock.Unlock()

	if !cu.modified {
		return nil
	}

	if err := write(ctx, dbConn, contractHash, assetCode, addressHash, cu.h); err != nil {
		return err
	}

	cu.modified = false
	return nil
}

func write(ctx context.Context, dbConn *db.DB, contractHash *bitcoin.Hash20,
	assetCode *protocol.AssetCode, addressHash *bitcoin.Hash20, h *state.Holding) error {

	data, err := serializeHolding(h)
	if err != nil {
		return errors.Wrap(err, "Failed to serialize holding")
	}

	if err := dbConn.Put(ctx, buildStoragePath(contractHash, assetCode, addressHash), data); err != nil {
		return err
	}

	return nil
}

// Returns the storage path prefix for a given identifier.
func buildStoragePath(contractHash *bitcoin.Hash20, assetCode *protocol.AssetCode, addressHash *bitcoin.Hash20) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s", storageKey, contractHash.String(), storageSubKey,
		assetCode.String(), addressHash.String())
}

func serializeHolding(h *state.Holding) ([]byte, error) {
	var buf bytes.Buffer

	// Version
	if err := binary.Write(&buf, binary.LittleEndian, uint8(0)); err != nil {
		return nil, err
	}

	if err := h.Address.Serialize(&buf); err != nil {
		return nil, err
	}

	if err := binary.Write(&buf, binary.LittleEndian, h.PendingBalance); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.LittleEndian, h.FinalizedBalance); err != nil {
		return nil, err
	}

	if err := h.CreatedAt.Serialize(&buf); err != nil {
		return nil, err
	}
	if err := h.UpdatedAt.Serialize(&buf); err != nil {
		return nil, err
	}

	if err := binary.Write(&buf, binary.LittleEndian, uint32(len(h.HoldingStatuses))); err != nil {
		return nil, err
	}

	for _, value := range h.HoldingStatuses {
		if err := serializeHoldingStatus(&buf, value); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

func serializeHoldingStatus(buf *bytes.Buffer, hs *state.HoldingStatus) error {
	if err := binary.Write(buf, binary.LittleEndian, hs.Code); err != nil {
		return err
	}

	if err := hs.Expires.Serialize(buf); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.LittleEndian, hs.Amount); err != nil {
		return err
	}

	if err := hs.TxId.Serialize(buf); err != nil {
		return err
	}

	if err := binary.Write(buf, binary.LittleEndian, hs.SettleQuantity); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.LittleEndian, hs.Posted); err != nil {
		return err
	}

	return nil
}

func serializeString(buf *bytes.Buffer, v []byte) error {
	if err := binary.Write(buf, binary.LittleEndian, uint32(len(v))); err != nil {
		return err
	}
	if _, err := buf.Write(v); err != nil {
		return err
	}
	return nil
}

func deserializeHolding(buf *bytes.Reader) (*state.Holding, error) {
	var result state.Holding

	// Version
	var version uint8
	if err := binary.Read(buf, binary.LittleEndian, &version); err != nil {
		return &result, err
	}
	if version != 0 {
		return &result, fmt.Errorf("Unknown version : %d", version)
	}

	err := result.Address.Deserialize(buf)
	if err != nil {
		return &result, err
	}

	if err := binary.Read(buf, binary.LittleEndian, &result.PendingBalance); err != nil {
		return &result, err
	}
	if err := binary.Read(buf, binary.LittleEndian, &result.FinalizedBalance); err != nil {
		return &result, err
	}
	result.CreatedAt, err = protocol.DeserializeTimestamp(buf)
	if err != nil {
		return &result, err
	}
	result.UpdatedAt, err = protocol.DeserializeTimestamp(buf)
	if err != nil {
		return &result, err
	}

	result.HoldingStatuses = make(map[protocol.TxId]*state.HoldingStatus)
	var length uint32
	if err := binary.Read(buf, binary.LittleEndian, &length); err != nil {
		return &result, err
	}
	for i := 0; i < int(length); i++ {
		var hs state.HoldingStatus
		if err := deserializeHoldingStatus(buf, &hs); err != nil {
			return &result, err
		}
		result.HoldingStatuses[*hs.TxId] = &hs
	}

	return &result, nil
}

func deserializeHoldingStatus(buf *bytes.Reader, hs *state.HoldingStatus) error {
	if err := binary.Read(buf, binary.LittleEndian, &hs.Code); err != nil {
		return err
	}

	var err error
	hs.Expires, err = protocol.DeserializeTimestamp(buf)
	if err != nil {
		return err
	}
	if err := binary.Read(buf, binary.LittleEndian, &hs.Amount); err != nil {
		return err
	}
	hs.TxId, err = protocol.DeserializeTxId(buf)
	if err != nil {
		return err
	}
	if err := binary.Read(buf, binary.LittleEndian, &hs.SettleQuantity); err != nil {
		return err
	}
	if err := binary.Read(buf, binary.LittleEndian, &hs.Posted); err != nil {
		return err
	}

	return nil
}

func deserializeString(buf *bytes.Reader) ([]byte, error) {
	var length uint32
	if err := binary.Read(buf, binary.LittleEndian, &length); err != nil {
		return nil, err
	}
	result := make([]byte, length)
	if _, err := buf.Read(result); err != nil {
		return nil, err
	}
	return result, nil
}
