package contest

import (
	"testing"

	"github.com/spikeekips/mitum/util"
	"github.com/stretchr/testify/suite"
)

type testStripNestedArray struct {
	suite.Suite
}

func (t *testStripNestedArray) printStriped(i interface{}) string {
	switch j := i.(type) {
	case []byte:
		return string(j)
	case string:
		return j
	default:
		b, err := util.MarshalJSON(i)
		t.NoError(err)

		return string(b)
	}
}

func (t *testStripNestedArray) TestNestedArray() {
	s := `
{
  "a": 1,
  "b": [ ["0", "1", "2"] , ["3", "4", "5"] ]
}
`

	i, err := stripNestedArray([]byte(s))
	t.NoError(err)

	t.T().Log("striped:", t.printStriped(i))

	j, ok := i.(map[string]interface{})
	t.True(ok, "%T", i)

	{
		a, found := j["a"]
		t.True(found)
		t.Equal(a, float64(1))
	}

	{
		b, found := j["b"]
		t.True(found)

		l, ok := b.([]interface{})
		t.True(ok)
		t.Equal(2, len(l))

		l0, ok := l[0].(map[string]interface{})
		t.True(ok, "%T", l[0])
		t.Equal("0", l0["0"])
		t.Equal("1", l0["1"])
		t.Equal("2", l0["2"])

		l1, ok := l[1].(map[string]interface{})
		t.True(ok, "%T", l[1])
		t.Equal("3", l1["0"])
		t.Equal("4", l1["1"])
		t.Equal("5", l1["2"])
	}
}

func (t *testStripNestedArray) TestInsideMap() {
	s := `
{
  "a": ["0", "1", "2"],
  "b": {
    "c": "c",
    "d": [ ["0", "1", "2"], ["3", "4", "5"], ["6", "7", "8"] ]
  }
}
`

	i, err := stripNestedArray([]byte(s))
	t.NoError(err)

	t.T().Log("striped:", t.printStriped(i))

	j, ok := i.(map[string]interface{})
	t.True(ok, "%T", i)

	{
		a, found := j["a"]
		t.True(found)

		l, ok := a.([]interface{})
		t.True(ok, "%T", a)
		t.Equal(3, len(l))

		t.True(ok, "%T", l)
		t.Equal("0", l[0])
		t.Equal("1", l[1])
		t.Equal("2", l[2])

	}

	{
		b, found := j["b"]
		t.True(found)

		m, ok := b.(map[string]interface{})
		t.True(ok, "%T", b)
		t.Equal(2, len(m))

		c, found := m["c"]
		t.True(found)
		t.Equal("c", c)

		d, found := m["d"]
		t.True(found)
		t.T().Log("d:", t.printStriped(d))

		l, ok := d.([]interface{})
		t.True(ok)
		t.Equal(3, len(l))

		l0, ok := l[0].(map[string]interface{})
		t.True(ok, "%T", l[0])
		t.Equal("0", l0["0"])
		t.Equal("1", l0["1"])
		t.Equal("2", l0["2"])

		l1, ok := l[1].(map[string]interface{})
		t.True(ok, "%T", l[1])
		t.Equal("3", l1["0"])
		t.Equal("4", l1["1"])
		t.Equal("5", l1["2"])

		l2, ok := l[2].(map[string]interface{})
		t.True(ok, "%T", l[2])
		t.Equal("6", l2["0"])
		t.Equal("7", l2["1"])
		t.Equal("8", l2["2"])
	}
}

func (t *testStripNestedArray) TestNotNestedArray() {
	s := `
{
  "a": 1,
  "b": ["0", "1", "3"]
}
`

	b, err := stripNestedArray([]byte(s))
	t.NoError(err)

	t.T().Log("striped:", t.printStriped(b))
}

func TestStripNestedArray(t *testing.T) {
	suite.Run(t, new(testStripNestedArray))
}
