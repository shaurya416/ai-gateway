// Package redisstub is an in-process Redis stand-in for tests: a RESP2 server
// on 127.0.0.1 implementing the handful of commands the response cache's Redis
// store sends — AUTH, SELECT, SET with PX or EX, GET, DEL and PING — with the
// faults a client has to survive injected on demand.
//
// It needs no Docker and no network beyond the loopback interface, so the
// store and the plugin above it are tested with the default `make test`. Expiry
// runs on a clock the test advances, so a test of PX never sleeps.
package redisstub

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// databases is how many logical databases SELECT accepts, Redis's default.
const databases = 16

type entry struct {
	value     []byte
	expiresAt time.Time // zero: no expiry
	px        int64     // the expiry as written, in milliseconds; 0 when none
}

// Server is a running stub. Every method is safe for concurrent use.
type Server struct {
	t testing.TB

	mu       sync.Mutex
	ln       net.Listener
	addr     string
	tls      *tls.Config
	roots    *x509.CertPool
	conns    map[net.Conn]struct{}
	dbs      [databases]map[string]entry
	username string
	password string
	offset   time.Duration
	stall    time.Duration
	truncate int
	fail     map[string]string
	commands []string
	accepted int
	stopped  chan struct{}

	wg sync.WaitGroup
}

// New starts a plaintext stub. It is stopped, and every connection it holds
// closed, when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	return start(t, nil, nil)
}

// NewTLS starts a stub that speaks TLS with a self-signed certificate for
// 127.0.0.1. RootCAs returns the pool a client verifies it against.
func NewTLS(t testing.TB) *Server {
	t.Helper()
	cert, roots := selfSigned(t)
	return start(t, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, roots)
}

func start(t testing.TB, tlsConf *tls.Config, roots *x509.CertPool) *Server {
	s := &Server{
		t:     t,
		tls:   tlsConf,
		roots: roots,
		conns: make(map[net.Conn]struct{}),
		fail:  make(map[string]string),
	}
	for i := range s.dbs {
		s.dbs[i] = make(map[string]entry)
	}
	s.listen("127.0.0.1:0")
	t.Cleanup(s.Close)
	return s
}

// listen binds addr and starts accepting on it.
func (s *Server) listen(addr string) {
	s.t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(s.t.Context(), "tcp", addr)
	if err != nil {
		s.t.Fatalf("redisstub: listen %s: %v", addr, err)
	}
	if s.tls != nil {
		ln = tls.NewListener(ln, s.tls)
	}
	s.mu.Lock()
	s.ln = ln
	s.addr = ln.Addr().String()
	s.stopped = make(chan struct{})
	stopped := s.stopped
	s.mu.Unlock()

	s.wg.Add(1)
	go s.accept(ln, stopped)
}

func (s *Server) accept(ln net.Listener, stopped chan struct{}) {
	defer s.wg.Done()
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		select {
		case <-stopped:
			s.mu.Unlock()
			_ = c.Close()
			return
		default:
		}
		s.conns[c] = struct{}{}
		s.accepted++
		s.mu.Unlock()

		s.wg.Add(1)
		go s.serve(c, stopped)
	}
}

// URL is the stub's address as a redis_url: redis://127.0.0.1:port, or
// rediss:// for a TLS stub, with no credentials and no database.
func (s *Server) URL() string {
	scheme := "redis"
	if s.tls != nil {
		scheme = "rediss"
	}
	return scheme + "://" + s.Addr()
}

// Addr is the host:port the stub listens on.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// RootCAs is the pool that verifies a TLS stub's certificate; nil for a
// plaintext stub.
func (s *Server) RootCAs() *x509.CertPool { return s.roots }

// RequireAuth makes every command but AUTH answer NOAUTH until the connection
// authenticates. An empty username accepts the one-argument AUTH password
// form as well as AUTH default password, as Redis does.
func (s *Server) RequireAuth(username, password string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.username, s.password = username, password
}

// Advance moves the stub's clock forward, expiring every entry whose PX or EX
// has elapsed.
func (s *Server) Advance(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.offset += d
}

// Stall delays every reply by d. Zero answers immediately again.
func (s *Server) Stall(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stall = d
}

// TruncateReplies makes the next n replies stop halfway — a bulk reply after
// its header and part of its payload — and closes the connection they were
// written to.
func (s *Server) TruncateReplies(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.truncate = n
}

// FailCommand makes every command named name (case-insensitive) answer with
// the error reply text, e.g. "OOM command not allowed". An empty text clears
// it.
func (s *Server) FailCommand(name, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name = strings.ToUpper(name)
	if text == "" {
		delete(s.fail, name)
		return
	}
	s.fail[name] = text
}

// Stop closes the listener and every open connection, so a dial is refused
// and a pooled connection reads EOF. The data survives; Restart serves it
// again on the same address.
func (s *Server) Stop() {
	s.mu.Lock()
	ln, stopped := s.ln, s.stopped
	s.ln = nil
	if stopped != nil {
		select {
		case <-stopped:
		default:
			close(stopped)
		}
	}
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
}

// Restart listens again on the address Stop gave up.
func (s *Server) Restart() {
	s.t.Helper()
	s.listen(s.Addr())
}

// DropConnections closes every open connection while the listener stays up:
// what a client sees when the server restarts between two of its commands, or
// reaps an idle connection.
func (s *Server) DropConnections() {
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// Close stops the stub and waits for every goroutine it started.
func (s *Server) Close() {
	s.Stop()
	s.wg.Wait()
}

// Commands lists the name of every command received, in order, upper-cased.
// Arguments are not recorded, so no value or credential is kept.
func (s *Server) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.commands)
}

// Accepted counts the connections the stub has accepted.
func (s *Server) Accepted() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepted
}

// Value returns the live value stored under key in database db.
func (s *Server) Value(db int, key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.liveLocked(db, key)
	return slices.Clone(e.value), ok
}

// PX returns the expiry key was last written with, in milliseconds.
func (s *Server) PX(db int, key string) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.liveLocked(db, key)
	return e.px, ok
}

// Keys lists the live keys in database db, sorted.
func (s *Server) Keys(db int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.dbs[db]))
	for k := range s.dbs[db] {
		if _, ok := s.liveLocked(db, k); ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

// Put stores value under key in database db with no expiry, bypassing the
// protocol: a test plants an entry another writer left.
func (s *Server) Put(db int, key string, value []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dbs[db][key] = entry{value: slices.Clone(value)}
}

func (s *Server) nowLocked() time.Time { return time.Now().Add(s.offset) }

func (s *Server) liveLocked(db int, key string) (entry, bool) {
	e, ok := s.dbs[db][key]
	if !ok {
		return entry{}, false
	}
	if !e.expiresAt.IsZero() && !s.nowLocked().Before(e.expiresAt) {
		delete(s.dbs[db], key)
		return entry{}, false
	}
	return e, true
}

// session is one client connection's state.
type session struct {
	db     int
	authed bool
}

func (s *Server) serve(c net.Conn, stopped chan struct{}) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		_ = c.Close()
	}()

	br := bufio.NewReader(c)
	var sess session
	for {
		args, err := readCommand(br)
		if err != nil {
			return
		}
		reply, stall, truncate := s.handle(&sess, args)
		if stall > 0 {
			timer := time.NewTimer(stall)
			select {
			case <-timer.C:
			case <-stopped:
				timer.Stop()
				return
			}
		}
		if truncate {
			_, _ = c.Write(reply[:max(1, len(reply)/2)])
			return
		}
		if _, err := c.Write(reply); err != nil {
			return
		}
	}
}

// handle executes one command and returns its encoded reply, the delay to
// apply before writing it, and whether to cut it short.
func (s *Server) handle(sess *session, args [][]byte) (reply []byte, stall time.Duration, truncate bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	name := strings.ToUpper(string(args[0]))
	s.commands = append(s.commands, name)
	stall = s.stall
	if s.truncate > 0 {
		s.truncate--
		truncate = true
	}
	if text, ok := s.fail[name]; ok {
		return errorReply(text), stall, truncate
	}
	if name != "AUTH" && s.password != "" && !sess.authed {
		return errorReply("NOAUTH Authentication required."), stall, truncate
	}
	return s.execLocked(sess, name, args[1:]), stall, truncate
}

func (s *Server) execLocked(sess *session, name string, args [][]byte) []byte {
	switch name {
	case "PING":
		return []byte("+PONG\r\n")
	case "AUTH":
		return s.authLocked(sess, args)
	case "SELECT":
		if len(args) != 1 {
			return arityError(name)
		}
		db, err := strconv.Atoi(string(args[0]))
		if err != nil || db < 0 || db >= databases {
			return errorReply("ERR DB index is out of range")
		}
		sess.db = db
		return []byte("+OK\r\n")
	case "GET":
		if len(args) != 1 {
			return arityError(name)
		}
		e, ok := s.liveLocked(sess.db, string(args[0]))
		if !ok {
			return []byte("$-1\r\n")
		}
		return bulk(e.value)
	case "SET":
		return s.setLocked(sess, args)
	case "DEL":
		if len(args) == 0 {
			return arityError(name)
		}
		removed := 0
		for _, k := range args {
			if _, ok := s.liveLocked(sess.db, string(k)); ok {
				delete(s.dbs[sess.db], string(k))
				removed++
			}
		}
		return []byte(":" + strconv.Itoa(removed) + "\r\n")
	default:
		return errorReply(fmt.Sprintf("ERR unknown command '%s'", name))
	}
}

func (s *Server) authLocked(sess *session, args [][]byte) []byte {
	var user, pass string
	switch len(args) {
	case 1:
		user, pass = "default", string(args[0])
	case 2:
		user, pass = string(args[0]), string(args[1])
	default:
		return arityError("AUTH")
	}
	if s.password == "" {
		return errorReply("ERR AUTH <password> called without any password configured for the default user.")
	}
	wantUser := s.username
	if wantUser == "" {
		wantUser = "default"
	}
	if user != wantUser || pass != s.password {
		sess.authed = false
		return errorReply("WRONGPASS invalid username-password pair or user is disabled.")
	}
	sess.authed = true
	return []byte("+OK\r\n")
}

func (s *Server) setLocked(sess *session, args [][]byte) []byte {
	if len(args) < 2 {
		return arityError("SET")
	}
	e := entry{value: slices.Clone(args[1])}
	for i := 2; i < len(args); i++ {
		opt := strings.ToUpper(string(args[i]))
		if (opt != "PX" && opt != "EX") || i+1 >= len(args) {
			return errorReply("ERR syntax error")
		}
		n, err := strconv.ParseInt(string(args[i+1]), 10, 64)
		if err != nil || n <= 0 {
			return errorReply("ERR invalid expire time in 'set' command")
		}
		ms := n
		if opt == "EX" {
			ms = n * 1000
		}
		e.px = ms
		e.expiresAt = s.nowLocked().Add(time.Duration(ms) * time.Millisecond)
		i++
	}
	s.dbs[sess.db][string(args[0])] = e
	return []byte("+OK\r\n")
}

func errorReply(text string) []byte { return []byte("-" + text + "\r\n") }

func arityError(name string) []byte {
	return errorReply(fmt.Sprintf("ERR wrong number of arguments for '%s' command", strings.ToLower(name)))
}

func bulk(b []byte) []byte {
	out := make([]byte, 0, len(b)+16)
	out = append(out, '$')
	out = strconv.AppendInt(out, int64(len(b)), 10)
	out = append(out, '\r', '\n')
	out = append(out, b...)
	return append(out, '\r', '\n')
}

// readCommand reads one RESP array of bulk strings, the only form a client
// library sends.
func readCommand(br *bufio.Reader) ([][]byte, error) {
	n, err := readHeader(br, '*')
	if err != nil {
		return nil, err
	}
	if n < 1 || n > 64 {
		return nil, fmt.Errorf("redisstub: command of %d arguments", n)
	}
	args := make([][]byte, n)
	for i := range args {
		size, err := readHeader(br, '$')
		if err != nil {
			return nil, err
		}
		if size < 0 || size > 512<<20 {
			return nil, fmt.Errorf("redisstub: argument of %d bytes", size)
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(br, buf); err != nil {
			return nil, err
		}
		if buf[size] != '\r' || buf[size+1] != '\n' {
			return nil, errors.New("redisstub: argument not terminated by CRLF")
		}
		args[i] = buf[:size]
	}
	return args, nil
}

func readHeader(br *bufio.Reader, kind byte) (int, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return 0, err
	}
	line = strings.TrimSuffix(line, "\r\n")
	if line == "" || line[0] != kind {
		return 0, fmt.Errorf("redisstub: expected %q header, got %q", kind, line)
	}
	return strconv.Atoi(line[1:])
}

// selfSigned makes a certificate for 127.0.0.1 and the pool that trusts it.
func selfSigned(t testing.TB) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("redisstub: generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "redisstub"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:              []string{"localhost"},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("redisstub: create certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("redisstub: parse certificate: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, roots
}
