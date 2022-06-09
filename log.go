package contest

import (
	"github.com/pkg/errors"
	"github.com/spikeekips/mitum/util"
	"go.mongodb.org/mongo-driver/bson"
)

type LogEntry interface {
	bson.Marshaler
	X()
}

type InternalLogEntry struct {
	msg string `bson:"msg"`
	err error  `bson:"error"`
}

func NewInternalLogEntry(msg string, err error) InternalLogEntry {
	return InternalLogEntry{msg: msg, err: err}
}

func (e InternalLogEntry) X() {}

type InternalLogEntryBSONMarshaler struct {
	Msg string `bson:"msg"`
	Err error  `bson:"error"`
}

func (e InternalLogEntry) MarshalBSON() ([]byte, error) {
	return bson.Marshal(InternalLogEntryBSONMarshaler{
		Msg: e.msg,
		Err: e.err,
	})
}

type NodeLogEntry struct {
	node string
	x    bson.Raw
}

func NewNodeLogEntry(node string, b []byte) (entry NodeLogEntry, _ error) {
	var x bson.Raw
	if len(b) > 0 {
		var u map[string]interface{}

		if err := util.UnmarshalJSON(b, &u); err != nil {
			return entry, errors.Wrap(err, "")
		}

		i, err := bson.Marshal(u)
		if err != nil {
			return entry, errors.Wrap(err, "")
		}

		x = i
	}

	return NodeLogEntry{node: node, x: x}, nil
}

func (e NodeLogEntry) X() {}

type NodeLogEntryBSONMarshaler struct {
	Node string   `bson:"node"`
	X    bson.Raw `bson:"x"`
}

func (e NodeLogEntry) MarshalBSON() ([]byte, error) {
	return bson.Marshal(NodeLogEntryBSONMarshaler{
		Node: e.node,
		X:    e.x,
	})
}
