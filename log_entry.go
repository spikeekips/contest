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
	t   time.Time
	err error  `bson:"error"`
	msg string `bson:"msg"`
}

func NewInternalLogEntry(msg string, err error) InternalLogEntry {
	return InternalLogEntry{t: localtime.Now().UTC(), msg: msg, err: err}
}

func (e InternalLogEntry) X() {}

type InternalLogEntryBSONMarshaler struct {
	T   time.Time `bson:"t"`
	Err error     `bson:"error"`
	ID  string    `bson:"_id"`
	Msg string    `bson:"msg"`
}

func (e InternalLogEntry) MarshalBSON() ([]byte, error) {
	return bson.Marshal(InternalLogEntryBSONMarshaler{
		ID:  util.ULID().String(),
		T:   e.t,
		Msg: e.msg,
		Err: e.err,
	})
}

type NodeLogEntry struct {
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
		t:      localtime.Now().UTC(),
		node:   node,
		x:      x,
		stderr: stderr,
	}, nil
}

func (e NodeLogEntry) X() {}

type NodeLogEntryBSONMarshaler struct {
	ID     string    `bson:"_id"`
	T      time.Time `bson:"t"`
	Node   string    `bson:"node"`
	X      bson.Raw  `bson:"x"`
	Stderr bool      `bson:"stderr"`
}

func (e NodeLogEntry) MarshalBSON() ([]byte, error) {
	return bson.Marshal(NodeLogEntryBSONMarshaler{
		ID:     util.ULID().String(),
		T:      e.t,
		Node:   e.node,
		X:      e.x,
		Stderr: e.stderr,
	})
}
