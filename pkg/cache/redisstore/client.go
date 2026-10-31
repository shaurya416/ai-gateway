package redisstore

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// ErrClosed is returned by an operation on a store that has been closed.
var ErrClosed = errors.New("redis: store closed")

// errNoConnection is returned when every connection stayed busy for the whole
// operation timeout. It reports itself as a timeout, which is what it is.
var errNoConnection error = &timeoutError{"redis: no connection became free within the timeout"}

type timeoutError struct{ msg string }

func (e *timeoutError) Error() string { return e.msg }
func (e *timeoutError) Timeout() bool { return true }

// client is a bounded pool of connections to one endpoint. It starts no
// goroutines: every connection is dialled by the operation that needs it, and
// every wait is bounded by that operation's deadline.
type client struct {
	ep      Endpoint
	timeout time.Duration
	tls     *tls.Config

	// slots holds one token per operation in flight, which is also the bound
	// on open connections: a connection is dialled only when none is idle, by
	// an operation that already holds a slot.
	slots chan struct{}
	done  chan struct{}

	mu     sync.Mutex
	idle   []*conn // most recently used last
	closed bool
}

func newClient(ep Endpoint, timeout time.Duration, poolSize int, tlsConf *tls.Config) *client {
	return &client{
		ep:      ep,
		timeout: timeout,
		tls:     tlsConf,
		slots:   make(chan struct{}, poolSize),
		done:    make(chan struct{}),
	}
}

// do runs one command, bounded end to end by the client's timeout: waiting for
// a free connection, dialling one, and the round trip.
//
// A command that fails on a connection taken from the idle pool with the
// connection itself gone — the server restarted, or reaped it while it sat
// idle — is sent once more on a fresh connection, inside the same deadline.
// Every command this client sends can be repeated safely: GET and DEL are
// reads or idempotent, and SET writes the same value twice.
func (cl *client) do(args ...[]byte) (reply, error) {
	deadline := time.Now().Add(cl.timeout)
	if err := cl.acquire(deadline); err != nil {
		return reply{}, err
	}
	defer func() { <-cl.slots }()

	cn, reused, err := cl.get(deadline)
	if err != nil {
		return reply{}, err
	}
	r, err := cn.roundTrip(deadline, args...)
	if err != nil && reused && connectionGone(err) {
		cn.close()
		if cn, err = cl.dial(deadline); err != nil {
			return reply{}, err
		}
		r, err = cn.roundTrip(deadline, args...)
	}
	var serr *ServerError
	if err != nil && !errors.As(err, &serr) {
		cn.close()
		return reply{}, err
	}
	cl.put(cn)
	return r, err
}

// connectionGone reports whether err says the connection was closed under the
// command, as opposed to the server answering, the reply being unreadable, or
// the deadline passing.
func connectionGone(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, net.ErrClosed)
}

func (cl *client) acquire(deadline time.Time) error {
	select {
	case <-cl.done:
		return ErrClosed
	default:
	}
	select {
	case cl.slots <- struct{}{}:
		return nil
	default:
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case cl.slots <- struct{}{}:
		return nil
	case <-timer.C:
		return errNoConnection
	case <-cl.done:
		return ErrClosed
	}
}

// get returns an idle connection, or dials one when none is idle. reused
// reports which.
func (cl *client) get(deadline time.Time) (cn *conn, reused bool, err error) {
	cl.mu.Lock()
	if n := len(cl.idle); n > 0 {
		cn = cl.idle[n-1]
		cl.idle = cl.idle[:n-1]
		cl.mu.Unlock()
		return cn, true, nil
	}
	cl.mu.Unlock()
	cn, err = cl.dial(deadline)
	return cn, false, err
}

// put returns a healthy connection to the idle pool, or closes it once the
// client is closed.
func (cl *client) put(cn *conn) {
	cl.mu.Lock()
	if !cl.closed && len(cl.idle) < cap(cl.slots) {
		cl.idle = append(cl.idle, cn)
		cl.mu.Unlock()
		return
	}
	cl.mu.Unlock()
	cn.close()
}

// dial opens a connection and prepares it: TLS for rediss://, then AUTH and
// SELECT as the URL asks.
func (cl *client) dial(deadline time.Time) (*conn, error) {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", cl.ep.Addr)
	if err != nil {
		return nil, err
	}
	if cl.ep.TLS {
		tc := tls.Client(nc, cl.tls)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = nc.Close()
			return nil, err
		}
		nc = tc
	}
	cn := newConn(nc)
	if err := cl.prepare(cn, deadline); err != nil {
		cn.close()
		return nil, err
	}
	return cn, nil
}

// prepare authenticates the connection and selects its database. The
// credentials never appear in an error: a refused AUTH is reported by the
// server's own message, which does not quote them.
func (cl *client) prepare(cn *conn, deadline time.Time) error {
	var auth [][]byte
	switch {
	case cl.ep.Username != "":
		auth = [][]byte{[]byte("AUTH"), []byte(cl.ep.Username), []byte(cl.ep.Password)}
	case cl.ep.Password != "":
		auth = [][]byte{[]byte("AUTH"), []byte(cl.ep.Password)}
	}
	if auth != nil {
		if err := expectOK(cn.roundTrip(deadline, auth...)); err != nil {
			return fmt.Errorf("redis AUTH: %w", err)
		}
	}
	if cl.ep.DB != 0 {
		if err := expectOK(cn.roundTrip(deadline, []byte("SELECT"), []byte(strconv.Itoa(cl.ep.DB)))); err != nil {
			return fmt.Errorf("redis SELECT %d: %w", cl.ep.DB, err)
		}
	}
	return nil
}

// expectOK turns a reply that should be +OK into an error when it is not.
func expectOK(r reply, err error) error {
	if err != nil {
		return err
	}
	if r.kind != '+' || string(r.str) != "OK" {
		return protocolErrorf("expected +OK")
	}
	return nil
}

// close closes every idle connection and refuses new operations. One in
// flight finishes, and its connection is closed rather than pooled.
func (cl *client) close() {
	cl.mu.Lock()
	if cl.closed {
		cl.mu.Unlock()
		return
	}
	cl.closed = true
	close(cl.done)
	idle := cl.idle
	cl.idle = nil
	cl.mu.Unlock()
	for _, cn := range idle {
		cn.close()
	}
}
