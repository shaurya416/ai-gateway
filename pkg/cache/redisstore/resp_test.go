package redisstore

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

func readFrom(raw string) (reply, error) {
	c := &conn{br: bufio.NewReaderSize(strings.NewReader(raw), readBufferSize)}
	return c.read()
}

func TestRead_ParsesEveryReplyTheClientExpects(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		raw  string
		want reply
	}{
		{"+OK\r\n", reply{kind: '+', str: []byte("OK")}},
		{":3\r\n", reply{kind: ':', num: 3}},
		{"$5\r\nhello\r\n", reply{kind: '$', str: []byte("hello")}},
		{"$0\r\n\r\n", reply{kind: '$', str: []byte{}}},
		{"$-1\r\n", reply{kind: '$', null: true}},
		// A payload may hold CR and LF; only the declared length counts.
		{"$4\r\na\r\nb\r\n", reply{kind: '$', str: []byte("a\r\nb")}},
	} {
		got, err := readFrom(tc.raw)
		if err != nil {
			t.Errorf("read(%q) = %v", tc.raw, err)
			continue
		}
		if got.kind != tc.want.kind || string(got.str) != string(tc.want.str) || got.num != tc.want.num || got.null != tc.want.null {
			t.Errorf("read(%q) = %+v, want %+v", tc.raw, got, tc.want)
		}
	}
}

func TestRead_ErrorReplyIsAServerError(t *testing.T) {
	t.Parallel()

	_, err := readFrom("-WRONGPASS invalid username-password pair\r\n")
	var serr *ServerError
	if !errors.As(err, &serr) || serr.Message != "WRONGPASS invalid username-password pair" {
		t.Fatalf("read = %v, want ServerError carrying the message", err)
	}
}

// A reply the client cannot follow is a protocol error — the connection is
// then out of step and is closed — and a reply that stops short is the
// connection's own error. Neither is mistaken for a value.
func TestRead_RefusesWhatItCannotFollow(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, raw string
		want      error
	}{
		{"array", "*1\r\n$1\r\na\r\n", errProtocol},
		{"unknown type", "!3\r\nabc\r\n", errProtocol},
		{"bare LF", "+OK\n", errProtocol},
		{"bulk past the bound", "$999999999999\r\n", errProtocol},
		{"negative bulk", "$-2\r\n", errProtocol},
		{"bulk without CRLF", "$2\r\nabXY", errProtocol},
		{"integer not a number", ":three\r\n", errProtocol},
		{"line past the buffer", "+" + strings.Repeat("x", readBufferSize) + "\r\n", errProtocol},
		{"empty line", "\r\n", errProtocol},
		{"cut before the line ends", "+O", io.EOF},
		{"cut inside the payload", "$10\r\nabc", io.ErrUnexpectedEOF},
	} {
		if _, err := readFrom(tc.raw); !errors.Is(err, tc.want) {
			t.Errorf("%s: read = %v, want %v", tc.name, err, tc.want)
		}
	}
}
