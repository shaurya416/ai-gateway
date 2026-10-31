package redisstore

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

const (
	// maxBulk bounds a bulk reply the client will buffer. A cached response is
	// kilobytes; a header announcing more than this is not an entry this store
	// wrote, or the stream is out of step, and reading it would pin that much
	// memory to one connection.
	maxBulk = 64 << 20
	// readBufferSize is the connection's read buffer, and so the longest status,
	// error or integer line accepted.
	readBufferSize = 16 << 10
	// maxRetainedWriteBuffer is the largest encode buffer a connection keeps
	// between commands, so one large entry does not pin its size for good.
	maxRetainedWriteBuffer = 64 << 10
)

// ServerError is an error reply from Redis, such as WRONGPASS or OOM. Redis
// answered in full, so the connection stays in step and is reused.
type ServerError struct {
	Message string
}

func (e *ServerError) Error() string { return "redis: " + e.Message }

// errProtocol marks a reply the client could not parse. The connection is out
// of step with the server from that point and is closed.
var errProtocol = errors.New("redis: protocol error")

func protocolErrorf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errProtocol, fmt.Sprintf(format, args...))
}

// reply is one RESP2 reply to a command. Arrays are not modelled: none of the
// commands this client sends answers with one.
type reply struct {
	kind byte   // '+' status, ':' integer, '$' bulk
	str  []byte // status text or bulk payload
	num  int64  // integer value
	null bool   // nil bulk: the key does not exist
}

// conn is one connection to Redis. It is used by one operation at a time.
type conn struct {
	nc   net.Conn
	br   *bufio.Reader
	wbuf []byte
}

func newConn(nc net.Conn) *conn {
	return &conn{nc: nc, br: bufio.NewReaderSize(nc, readBufferSize)}
}

func (c *conn) close() { _ = c.nc.Close() }

// roundTrip sends one command and reads its reply, all before deadline. An
// error reply is returned as a *ServerError alongside the zero reply.
func (c *conn) roundTrip(deadline time.Time, args ...[]byte) (reply, error) {
	if err := c.nc.SetDeadline(deadline); err != nil {
		return reply{}, err
	}
	if err := c.write(args); err != nil {
		return reply{}, err
	}
	return c.read()
}

// write encodes args as a RESP array of bulk strings and sends it in one
// write.
func (c *conn) write(args [][]byte) error {
	buf := c.wbuf[:0]
	buf = append(buf, '*')
	buf = strconv.AppendInt(buf, int64(len(args)), 10)
	buf = append(buf, '\r', '\n')
	for _, arg := range args {
		buf = append(buf, '$')
		buf = strconv.AppendInt(buf, int64(len(arg)), 10)
		buf = append(buf, '\r', '\n')
		buf = append(buf, arg...)
		buf = append(buf, '\r', '\n')
	}
	if cap(buf) <= maxRetainedWriteBuffer {
		c.wbuf = buf
	} else {
		c.wbuf = nil
	}
	_, err := c.nc.Write(buf)
	return err
}

// read parses one reply.
func (c *conn) read() (reply, error) {
	line, err := c.readLine()
	if err != nil {
		return reply{}, err
	}
	if len(line) == 0 {
		return reply{}, protocolErrorf("empty reply line")
	}
	switch kind, body := line[0], line[1:]; kind {
	case '+':
		return reply{kind: '+', str: append([]byte(nil), body...)}, nil
	case '-':
		return reply{}, &ServerError{Message: string(body)}
	case ':':
		n, err := strconv.ParseInt(string(body), 10, 64)
		if err != nil {
			return reply{}, protocolErrorf("integer reply is not a number")
		}
		return reply{kind: ':', num: n}, nil
	case '$':
		return c.readBulk(body)
	case '*':
		return reply{}, protocolErrorf("unexpected array reply")
	default:
		return reply{}, protocolErrorf("unknown reply type %q", kind)
	}
}

// readBulk reads the payload of a bulk reply whose header line was body.
func (c *conn) readBulk(body []byte) (reply, error) {
	n, err := strconv.Atoi(string(body))
	switch {
	case err != nil:
		return reply{}, protocolErrorf("bulk length is not a number")
	case n == -1:
		return reply{kind: '$', null: true}, nil
	case n < 0 || n > maxBulk:
		return reply{}, protocolErrorf("bulk length %d is out of range", n)
	}
	buf := make([]byte, n+2)
	if _, err := io.ReadFull(c.br, buf); err != nil {
		return reply{}, err
	}
	if buf[n] != '\r' || buf[n+1] != '\n' {
		return reply{}, protocolErrorf("bulk reply not terminated by CRLF")
	}
	return reply{kind: '$', str: buf[:n]}, nil
}

// readLine returns one CRLF-terminated line without its terminator. The slice
// is valid until the next read.
func (c *conn) readLine() ([]byte, error) {
	line, err := c.br.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return nil, protocolErrorf("reply line longer than %d bytes", readBufferSize)
		}
		return nil, err
	}
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return nil, protocolErrorf("reply line not terminated by CRLF")
	}
	return line[:len(line)-2], nil
}
