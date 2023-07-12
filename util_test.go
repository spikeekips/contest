package contest

import (
	"bytes"
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/suite"
)

func TestBytesLines(tt *testing.T) {
	t := new(suite.Suite)
	t.SetT(tt)

	cases := []struct {
		name  string
		input [][]byte
		lines [][]byte
		left  []byte
	}{
		{
			name: "no left",
			input: [][]byte{
				[]byte("showme"),
				[]byte("findme"),
				[]byte("killme\n"),
			},
			lines: [][]byte{
				[]byte("showme"),
				[]byte("findme"),
				[]byte("killme"),
			},
		},
		{
			name: "left",
			input: [][]byte{
				[]byte("showme"),
				[]byte("findme"),
				[]byte("killme"),
			},
			lines: [][]byte{
				[]byte("showme"),
				[]byte("findme"),
			},
			left: []byte("killme"),
		},
		{
			name: "no lines",
			input: [][]byte{
				[]byte("showme"),
			},
			left: []byte("showme"),
		},
		{
			name: "empty",
		},
	}

	for i, c := range cases {
		i := i
		c := c
		t.Run(c.name, func() {
			var lines [][]byte
			left, err := BytesLines(bytes.Join(c.input, []byte{'\n'}), func(b []byte) error {
				lines = append(lines, b)

				return nil
			})
			t.NoError(err)

			t.Equal(c.lines, lines, "%d(%q): lines", i, c.name)
			t.Equal(c.left, left, "%d(%q): left", i, c.name)
		})
	}

	t.Run("error", func() {
		left, err := BytesLines([]byte("f\nindme"), func([]byte) error {
			return errors.Errorf("showme")
		})
		t.Error(err)
		t.Nil(left)
		t.ErrorContains(err, "showme")
	})
}
