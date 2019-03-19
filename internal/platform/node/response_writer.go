package node

import (
	"context"

	"github.com/btcsuite/btcutil"
	"github.com/tokenized/smart-contract/internal/platform/protomux"
	"github.com/tokenized/smart-contract/pkg/inspector"
	"github.com/tokenized/smart-contract/pkg/wire"
)

type ResponseWriter struct {
	Inputs  []inspector.UTXO
	Outputs []Output
	Config  *Config
	Mux     protomux.Handler
}

func (w *ResponseWriter) Respond(ctx context.Context, tx *wire.MsgTx) error {
	return w.Mux.Respond(ctx, tx)
}

// Output is an output address for a response
type Output struct {
	Address btcutil.Address
	Value   uint64
	Change  bool
}

// OutputFee prepares a special fee output based on node configuration
func OutputFee(ctx context.Context, config *Config) *Output {
	if config.FeeValue > 0 {
		feeAddr, _ := btcutil.DecodeAddress(config.FeeAddress, &config.ChainParams)
		return &Output{
			Address: feeAddr,
			Value:   config.FeeValue,
		}
	}

	return nil
}
