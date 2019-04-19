package tests

import (
	"context"
	"os"
	"testing"

	"github.com/tokenized/smart-contract/cmd/smartcontractd/handlers"
	"github.com/tokenized/smart-contract/cmd/smartcontractd/listeners"
	"github.com/tokenized/smart-contract/internal/platform/node"
	"github.com/tokenized/smart-contract/internal/platform/protomux"
	"github.com/tokenized/smart-contract/internal/platform/tests"
	"github.com/tokenized/smart-contract/internal/platform/wallet"
	"github.com/tokenized/smart-contract/pkg/inspector"
	"github.com/tokenized/specification/dist/golang/protocol"

	"github.com/tokenized/smart-contract/pkg/wire"
)

var a *node.App
var test *tests.Test

// Information about the handlers we have created for testing.
var responses []*wire.MsgTx

var userKey *wallet.RootKey
var issuerKey *wallet.RootKey

var testTokenQty uint64
var testAssetCode protocol.AssetCode
var testVoteTxId protocol.TxId
var testVoteResultTxId protocol.TxId
var testFreezeTxId protocol.TxId

// TestMain is the entry point for testing.
func TestMain(m *testing.M) {
	os.Exit(testMain(m))
}

func testMain(m *testing.M) int {
	test = tests.New()
	defer test.TearDown()

	// =========================================================================
	// Locals

	testTokenQty = 1000

	// =========================================================================
	// API

	tracer := listeners.NewTracer()

	a, err := handlers.API(
		test.Context,
		test.Wallet,
		&test.NodeConfig,
		test.MasterDB,
		tracer,
		test.Scheduler,
		nil,
		test.UTXOs,
	)

	if err != nil {
		panic(err)
	}

	a.SetResponder(respondTx)

	// =========================================================================
	// Keys

	userKey, err = tests.GenerateKey(test.NodeConfig.ChainParams)
	if err != nil {
		panic(err)
	}

	issuerKey, err = tests.GenerateKey(test.NodeConfig.ChainParams)
	if err != nil {
		panic(err)
	}

	return m.Run()
}

func respondTx(ctx context.Context, tx *wire.MsgTx) error {
	responses = append(responses, tx)
	return nil
}

func checkResponse(t *testing.T, responseCode string) {
	ctx := test.Context

	if len(responses) != 1 {
		t.Fatalf("\t%s\t%s Response not created", tests.Failed, responseCode)
	}

	response := responses[0]
	responses = nil
	var responseMsg protocol.OpReturnMessage
	var err error
	for _, output := range response.TxOut {
		responseMsg, err = protocol.Deserialize(output.PkScript)
		if err == nil {
			break
		}
	}
	if responseMsg == nil {
		t.Fatalf("\t%s\t%s Response doesn't contain tokenized op return", tests.Failed, responseCode)
	}
	if responseMsg.Type() != responseCode {
		t.Fatalf("\t%s\tResponse is the wrong type : %s != %s", tests.Failed, responseMsg.Type(), responseCode)
	}

	// Submit response
	responseItx, err := inspector.NewTransactionFromWire(ctx, response)
	if err != nil {
		t.Fatalf("\t%s\tFailed to create response itx : %v", tests.Failed, err)
	}

	err = responseItx.Promote(ctx, test.RPCNode)
	if err != nil {
		t.Fatalf("\t%s\tFailed to promote response itx : %v", tests.Failed, err)
	}

	test.RPCNode.AddTX(ctx, response)

	err = a.Trigger(ctx, protomux.SEE, responseItx)
	if err != nil {
		t.Fatalf("\t%s\tFailed to process response : %v", tests.Failed, err)
	}

	if len(responses) != 0 {
		t.Fatalf("\t%s\tResponse created a response", tests.Failed)
	}

	t.Logf("\t%s\tResponse processed : %s", tests.Success, responseCode)
}
