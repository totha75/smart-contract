package protomux

import (
	"context"
	"errors"

	"github.com/tokenized/smart-contract/pkg/inspector"
	"github.com/tokenized/smart-contract/pkg/wire"
)

const (
	// SEE is used for broadcast txs
	SEE = "SEE"

	// SAW is used for replayed txs (reserved)
	SAW = "SAW"

	// LOST is used for reorgs
	LOST = "LOST"

	// STOLE is used for double spends
	STOLE = "STOLE"

	// REPROCESS is used to call back finalization of a tx
	REPROCESS = "REPROCESS"
)

// Handler is the interface for this Protocol Mux
type Handler interface {
	Respond(context.Context, wire.Message) error
	Trigger(context.Context, string, *inspector.Transaction) error
	SetResponder(ResponderFunc)
}

// A Handler is a type that handles a protocol messages
type HandlerFunc func(ctx context.Context, itx *inspector.Transaction, pkhs []string) error

// A ResponderFunc will handle responses
type ResponderFunc func(ctx context.Context, tx *wire.MsgTx) error

type ProtoMux struct {
	Responder         ResponderFunc
	SeeHandlers       map[string][]HandlerFunc
	LostHandlers      map[string][]HandlerFunc
	StoleHandlers     map[string][]HandlerFunc
	ReprocessHandlers map[string][]HandlerFunc

	SeeDefaultHandlers       []HandlerFunc
	LostDefaultHandlers      []HandlerFunc
	StoleDefaultHandlers     []HandlerFunc
	ReprocessDefaultHandlers []HandlerFunc
}

func New() *ProtoMux {
	pm := &ProtoMux{
		SeeHandlers:       make(map[string][]HandlerFunc),
		LostHandlers:      make(map[string][]HandlerFunc),
		StoleHandlers:     make(map[string][]HandlerFunc),
		ReprocessHandlers: make(map[string][]HandlerFunc),
	}

	return pm
}

// Handle registers a new handler
func (p *ProtoMux) Handle(verb, event string, handler HandlerFunc) {
	switch verb {
	case SEE:
		p.SeeHandlers[event] = append(p.SeeHandlers[event], handler)
	case LOST:
		p.LostHandlers[event] = append(p.LostHandlers[event], handler)
	case STOLE:
		p.StoleHandlers[event] = append(p.StoleHandlers[event], handler)
	case REPROCESS:
		p.ReprocessHandlers[event] = append(p.ReprocessHandlers[event], handler)
	default:
		panic("Unknown handler type")
	}
}

// Handle registers a new default handler
func (p *ProtoMux) HandleDefault(verb string, handler HandlerFunc) {
	switch verb {
	case SEE:
		p.SeeDefaultHandlers = append(p.SeeDefaultHandlers, handler)
	case LOST:
		p.LostDefaultHandlers = append(p.LostDefaultHandlers, handler)
	case STOLE:
		p.StoleDefaultHandlers = append(p.StoleDefaultHandlers, handler)
	case REPROCESS:
		p.ReprocessDefaultHandlers = append(p.ReprocessDefaultHandlers, handler)
	default:
		panic("Unknown handler type")
	}
}

// Trigger fires a handler
func (p *ProtoMux) Trigger(ctx context.Context, verb string, itx *inspector.Transaction) error {

	if itx.MsgProto == nil {
		return errors.New("Not a protocol tx")
	}

	var group map[string][]HandlerFunc

	switch verb {
	case SEE:
		group = p.SeeHandlers
	case LOST:
		group = p.LostHandlers
	case STOLE:
		group = p.StoleHandlers
	case REPROCESS:
		group = p.ReprocessHandlers
	default:
		return errors.New("Unknown handler type")
	}

	// Locate the handlers from the group
	txAction := itx.MsgProto.Type()
	handlers, exists := group[txAction]

	if !exists {
		switch verb {
		case SEE:
			handlers = p.SeeDefaultHandlers
		case LOST:
			handlers = p.LostDefaultHandlers
		case STOLE:
			handlers = p.StoleDefaultHandlers
		case REPROCESS:
			handlers = p.ReprocessDefaultHandlers
		default:
			return errors.New("Unknown handler type")
		}
	}

	// Find contract PKHs
	var pkhs []string
	for _, addr := range itx.ContractAddresses() {
		pkhs = append(pkhs, addr.String())
	}

	// Notify the listeners
	for _, listener := range handlers {
		if err := listener(ctx, itx, pkhs); err != nil {
			return err
		}
	}

	return nil
}

// SetResponder sets the function used for handling responses
func (p *ProtoMux) SetResponder(responder ResponderFunc) {
	p.Responder = responder
}

// Respond handles a response via the responder
func (p *ProtoMux) Respond(ctx context.Context, m wire.Message) error {
	tx, ok := m.(*wire.MsgTx)
	if !ok {
		return errors.New("Could not assert as *wire.MsgTx")
	}

	return p.Responder(ctx, tx)
}
