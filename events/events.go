package events

import (
	"context"
	"errors"
	"fmt"

	"github.com/bluesky-social/indigo/util"
	logging "github.com/ipfs/go-log"
	"go.opentelemetry.io/otel"
)

var log = logging.Logger("events")

type EventManager struct {
	subs []*Subscriber

	ops        chan *Operation
	closed     chan struct{}
	bufferSize int

	persister EventPersistence
}

func NewEventManager(persister EventPersistence) *EventManager {
	return &EventManager{
		ops:        make(chan *Operation),
		closed:     make(chan struct{}),
		bufferSize: 1024,
		persister:  persister,
	}
}

const (
	opSubscribe = iota
	opUnsubscribe
	opSend
)

type Operation struct {
	op  int
	sub *Subscriber
	evt *XRPCStreamEvent
}

func (em *EventManager) Run() {
	for op := range em.ops {
		switch op.op {
		case opSubscribe:
			em.subs = append(em.subs, op.sub)
		case opUnsubscribe:
			for i, s := range em.subs {
				if s == op.sub {
					em.subs[i] = em.subs[len(em.subs)-1]
					em.subs = em.subs[:len(em.subs)-1]
					break
				}
			}
		case opSend:
			if err := em.persister.Persist(context.TODO(), op.evt); err != nil {
				log.Errorf("failed to persist outbound event: %s", err)
			}

			for _, s := range em.subs {
				if s.filter(op.evt) {
					select {
					case s.outgoing <- op.evt:
					default:
						log.Warnf("event overflow (%d)", len(s.outgoing))
					}
				}
			}
		default:
			log.Errorf("unrecognized eventmgr operation: %d", op.op)
		}
	}
}

type Subscriber struct {
	outgoing chan *XRPCStreamEvent

	filter func(*XRPCStreamEvent) bool

	done chan struct{}
}

const (
	EvtKindErrorFrame = -1
	EvtKindRepoAppend = 1
	EvtKindInfoFrame  = 2
	EvtKindLabelBatch = 3
)

type EventHeader struct {
	Op int64 `cborgen:"op"`
}

type XRPCStreamEvent struct {
	RepoAppend *RepoAppend
	Info       *InfoFrame
	Error      *ErrorFrame
	LabelBatch *LabelBatch

	// some private fields for internal routing perf
	PrivUid         util.Uid `json:"-" cborgen:"-"`
	PrivPdsId       uint     `json:"-" cborgen:"-"`
	PrivRelevantPds []uint   `json:"-" cborgen:"-"`
}

type RepoAppend struct {
	Seq int64 `cborgen:"seq"`

	Event string `cborgen:"event"`

	// Repo is the DID of the repo this event is about
	Repo string `cborgen:"repo"`

	Commit string  `cborgen:"commit"`
	Prev   *string `cborgen:"prev"`
	//Commit cid.Cid  `cborgen:"commit"`
	//Prev   *cid.Cid `cborgen:"prev"`

	Ops    []*RepoOp `cborgen:"ops"`
	Blocks []byte    `cborgen:"blocks"`
	TooBig bool      `cborgen:"tooBig"`

	Blobs []string `cborgen:"blobs"`

	Time string `cborgen:"time"`
}

type RepoOp struct {
	Path   string  `cborgen:"path"`
	Action string  `cborgen:"action"`
	Cid    *string `cborgen:"cid"`
}

type LabelBatch struct {
	Seq    int64   `cborgen:"seq"`
	Labels []Label `cborgen:"labels"`
}

type InfoFrame struct {
	Info    string `cborgen:"info"`
	Message string `cborgen:"message"`
}

type ErrorFrame struct {
	Error   string `cborgen:"error"`
	Message string `cborgen:"message"`
}

func (em *EventManager) AddEvent(ctx context.Context, ev *XRPCStreamEvent) error {
	ctx, span := otel.Tracer("events").Start(ctx, "AddEvent")
	defer span.End()

	select {
	case em.ops <- &Operation{
		op:  opSend,
		evt: ev,
	}:
		return nil
	case <-em.closed:
		return fmt.Errorf("event manager shut down")
	}
}

func (em *EventManager) AddLabelEvent(ev *XRPCStreamEvent) error {
	select {
	case em.ops <- &Operation{
		op:  opSend,
		evt: ev,
	}:
		return nil
	case <-em.closed:
		return fmt.Errorf("event manager shut down")
	}
}

var ErrPlaybackShutdown = fmt.Errorf("playback shutting down")

func (em *EventManager) Subscribe(ctx context.Context, filter func(*XRPCStreamEvent) bool, since *int64) (<-chan *XRPCStreamEvent, func(), error) {
	if filter == nil {
		filter = func(*XRPCStreamEvent) bool { return true }
	}

	done := make(chan struct{})
	sub := &Subscriber{
		outgoing: make(chan *XRPCStreamEvent, em.bufferSize),
		filter:   filter,
		done:     done,
	}

	go func() {
		if since != nil {
			if err := em.persister.Playback(ctx, *since, func(e *XRPCStreamEvent) error {
				select {
				case <-done:
					return ErrPlaybackShutdown
				case sub.outgoing <- e:
					return nil
				}
			}); err != nil {
				if errors.Is(err, ErrPlaybackShutdown) {
					log.Warnf("events playback: %s", err)
				} else {
					log.Errorf("events playback: %s", err)
				}
			}
		}

		select {
		case em.ops <- &Operation{
			op:  opSubscribe,
			sub: sub,
		}:
		case <-em.closed:
			log.Errorf("failed to subscribe, event manager shut down")
		}
	}()

	cleanup := func() {
		close(done)
		select {
		case em.ops <- &Operation{
			op:  opUnsubscribe,
			sub: sub,
		}:
		case <-em.closed:
		}
	}

	return sub.outgoing, cleanup, nil
}
