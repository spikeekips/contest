package contest

import (
	"fmt"
	"reflect"
	"regexp"
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

	switch i, err := bsonStripNestedArray(b); {
	case err == nil:
		x = i
	default:
		j, err := bson.Marshal(bson.M{"text": string(b)})
		if err != nil {
			return entry, errors.WithStack(err)
		}

		x = j
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

var reNestedArrayStart = regexp.MustCompile(`\[\s*\[`)

func bsonStripNestedArray(b []byte) (bson.Raw, error) {
	i, err := stripNestedArray(b)
	if err != nil {
		return nil, err
	}

	j, err := bson.Marshal(i)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return bson.Raw(j), nil
}

// stripNestedArray removes nested slice; ferretdb can not handle nested array.
// For more details, see https://docs.ferretdb.io/diff/ .
func stripNestedArray(b []byte) (interface{}, error) {
	var u map[string]interface{}
	if err := util.UnmarshalJSON(b, &u); err != nil {
		return nil, err
	}

	if reNestedArrayStart.FindIndex(b) == nil {
		return u, nil
	}

	return convertNestedArray(reflect.ValueOf(u), false).Interface(), nil
}

func convertNestedArray(v reflect.Value, nested bool) reflect.Value { //revive:disable-line:flag-parameter
	vi := reflect.ValueOf(v.Interface())

	switch vi.Kind() {
	case reflect.Slice, reflect.Array:
		if vi.Len() < 1 {
			return vi
		}

		if nested {
			m := map[string]interface{}{}

			for i := 0; i < vi.Len(); i++ {
				m[fmt.Sprintf("%d", i)] = convertNestedArray(vi.Index(i), false).Interface()
			}

			return reflect.ValueOf(m)
		}

		a := make([]interface{}, vi.Len())
		for i := 0; i < vi.Len(); i++ {
			a[i] = convertNestedArray(vi.Index(i), true).Interface()
		}

		return reflect.ValueOf(a)
	case reflect.Map:
		if vi.Len() < 1 {
			return vi
		}

		mv := reflect.MakeMapWithSize(
			reflect.MapOf(vi.Type().Key(), vi.Type().Elem()),
			vi.Len(),
		)

		keys := vi.MapKeys()

		for i := range keys {
			j := convertNestedArray(vi.MapIndex(keys[i]), false)
			mv.SetMapIndex(keys[i], j)
		}

		return mv
	default:
		return vi
	}
}
