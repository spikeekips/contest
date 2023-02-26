package contest

import (
	"time"

	"github.com/pkg/errors"
	"github.com/spikeekips/mitum/util"
	"github.com/spikeekips/mitum/util/localtime"
	"go.mongodb.org/mongo-driver/bson"
)

type LogEntry interface {
	bson.Marshaler
	X()
}

type InternalLogEntry struct {
	id  string
	t   time.Time
	err error
	msg string
}

func NewInternalLogEntry(msg string, err error) InternalLogEntry {
	return InternalLogEntry{id: util.ULID().String(), t: localtime.Now().UTC(), msg: msg, err: err}
}

func (InternalLogEntry) X() {}

type InternalLogEntryBSONMarshaler struct {
	T   time.Time `bson:"t"`
	Err error     `bson:"error"` //nolint:tagliatelle //...
	ID  string    `bson:"_id"`   //nolint:tagliatelle //...
	Msg string    `bson:"msg"`
}

func (e InternalLogEntry) MarshalBSON() ([]byte, error) {
	b, err := bson.Marshal(InternalLogEntryBSONMarshaler{
		ID:  e.id,
		T:   e.t,
		Msg: e.msg,
		Err: e.err,
	})

	return b, errors.WithStack(err)
}

type NodeLogEntry struct {
	id     string
	t      time.Time
	node   string
	x      bson.Raw
	stderr bool
}

func NewNodeLogEntryWithInterface(node string, stderr bool, i interface{}) (entry NodeLogEntry, _ error) {
	var x bson.Raw

	if i != nil {
		switch t := i.(type) {
		case []byte:
			return NewNodeLogEntry(node, stderr, t)
		default:
			b, err := bson.Marshal(t)
			if err != nil {
				return entry, errors.WithStack(err)
			}

			x = b
		}
	}

	return NodeLogEntry{
		id:     util.ULID().String(),
		t:      localtime.Now().UTC(),
		node:   node,
		x:      x,
		stderr: stderr,
	}, nil
}

func NewNodeLogEntry(node string, stderr bool, b []byte) (entry NodeLogEntry, _ error) {
	var x bson.Raw

	if len(b) > 0 {
		var u map[string]interface{}

		switch err := util.UnmarshalJSON(b, &u); {
		case err == nil:
		default:
			u = bson.M{"text": string(b)}
		}

		i, err := bson.Marshal(u)
		if err != nil {
			return entry, errors.WithStack(err)
		}

		x = i
	}

	return NodeLogEntry{
		id:     util.ULID().String(),
		t:      localtime.Now().UTC(),
		node:   node,
		x:      x,
		stderr: stderr,
	}, nil
}

func (NodeLogEntry) X() {}

type NodeLogEntryBSONMarshaler struct {
	ID     string    `bson:"_id"` //nolint:tagliatelle //...
	T      time.Time `bson:"t"`
	Node   string    `bson:"node"`
	X      bson.Raw  `bson:"x"`
	Stderr bool      `bson:"stderr"`
}

func (e NodeLogEntry) MarshalBSON() ([]byte, error) {
	b, err := bson.Marshal(NodeLogEntryBSONMarshaler{
		ID:     e.id,
		T:      e.t,
		Node:   e.node,
		X:      e.x,
		Stderr: e.stderr,
	})

	return b, errors.WithStack(err)
}
