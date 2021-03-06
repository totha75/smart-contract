package data

import (
	"context"
	"sync"
	"time"

	"github.com/tokenized/smart-contract/pkg/bitcoin"
	"github.com/tokenized/smart-contract/pkg/logger"
	"github.com/tokenized/smart-contract/pkg/wire"

	"github.com/pkg/errors"
)

// TxTracker saves txids that have been announced by a specific node, but not requested yet.
// For example, the node announces the tx, but another node has already requested it.
// But if the node it was requested from never actually gives the tx, then it needs to be
//   re-requested from a node that has previously announced it.
// So TxTracker remembers all the txids that we don't have the tx for so we can re-request if
//   necessary.

// When a block is confirmed, its txids are fed back through an interface to the node class which
//   uses that data to call back to all active TxTrackers to remove any txs being tracked that are
//   now confirmed.

type TxTracker struct {
	txids map[bitcoin.Hash32]time.Time
	mutex sync.Mutex
}

func NewTxTracker() *TxTracker {
	result := TxTracker{
		txids: make(map[bitcoin.Hash32]time.Time),
	}

	return &result
}

// Adds a txid to tracker to be monitored for expired requests
func (tracker *TxTracker) Add(txid *bitcoin.Hash32) {
	tracker.mutex.Lock()
	defer tracker.mutex.Unlock()

	if _, exists := tracker.txids[*txid]; !exists {
		tracker.txids[*txid] = time.Now()
	}
}

// Call when a tx is received to cancel tracking
func (tracker *TxTracker) Remove(ctx context.Context, txids []*bitcoin.Hash32) {
	tracker.mutex.Lock()
	defer tracker.mutex.Unlock()

	// Iterate tracker ids first since that list should be much smaller
	for _, removeid := range txids {
		if time, exists := tracker.txids[*removeid]; exists {
			logger.Verbose(ctx, "Removing tracking for tx (%s) : %s", time.Format("15:04:05.999999"), removeid.String())
			delete(tracker.txids, *removeid)
		}
	}
}

// Called periodically to request any txs that have not been received yet
func (tracker *TxTracker) Check(ctx context.Context, mempool *MemPool) ([]wire.Message, error) {
	tracker.mutex.Lock()
	defer tracker.mutex.Unlock()

	response := []wire.Message{}
	invRequest := wire.NewMsgGetData()
	for txid, _ := range tracker.txids {
		newTxId := txid // Make a copy to ensure the value isn't overwritten by the next iteration
		alreadyHave, shouldRequest := mempool.AddRequest(&newTxId)
		if alreadyHave {
			delete(tracker.txids, txid) // Remove since we have received tx
		} else {
			if shouldRequest {
				logger.Verbose(ctx, "Re-Requesting tx : %s", newTxId.String())
				item := wire.NewInvVect(wire.InvTypeTx, &newTxId)
				// Request
				if err := invRequest.AddInvVect(item); err != nil {
					// Too many requests for one message
					response = append(response, invRequest) // Append full message
					invRequest = wire.NewMsgGetData()       // Start new message

					// Try to add it again
					if err := invRequest.AddInvVect(item); err != nil {
						return response, errors.Wrap(err, "Failed to add tx to get data request")
					} else {
						delete(tracker.txids, txid) // Remove since we requested
					}
				} else {
					delete(tracker.txids, txid) // Remove since we requested
				}
			} // else wait and check again later
		}
	}

	if len(invRequest.InvList) > 0 {
		response = append(response, invRequest)
	}

	return response, nil
}
