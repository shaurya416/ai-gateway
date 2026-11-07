package redisstore

import (
	"bufio"
	"bytes"
	"errors"
	"strconv"
	"testing"
)

// FuzzRead feeds arbitrary bytes to the RESP2 reply reader. A reply is either
// parsed into exactly what its header announced, or refused as a protocol,
// server or read error; it never panics, and a bulk payload is never longer
// than the bound.
func FuzzRead(f *testing.F) {
	for _, seed := range []string{
		"+OK\r\n", ":3\r\n", "$5\r\nhello\r\n", "$0\r\n\r\n", "$-1\r\n",
		"$4\r\na\r\nb\r\n", "-WRONGPASS invalid\r\n", "*1\r\n", "$5\r\nhel", "+OK\n", "",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		c := &conn{br: bufio.NewReaderSize(bytes.NewReader(data), readBufferSize)}
		got, err := c.read()
		if err != nil {
			var serverErr *ServerError
			if got.kind != 0 && !errors.As(err, &serverErr) {
				t.Fatalf("read returned a reply alongside %v", err)
			}
			return
		}
		switch got.kind {
		case '+', ':':
		case '$':
			if got.null {
				return
			}
			line, _, _ := bytes.Cut(data, []byte("\r\n"))
			n, convErr := strconv.Atoi(string(line[1:]))
			if convErr != nil || n != len(got.str) || len(got.str) > maxBulk {
				t.Fatalf("bulk reply of %d bytes does not match its header %q", len(got.str), line)
			}
		default:
			t.Fatalf("read accepted reply kind %q", got.kind)
		}
	})
}
