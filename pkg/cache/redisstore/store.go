// Package redisstore is a Redis-backed store for the response-cache plugin, so
// every gateway replica pointed at one Redis serves the entries any of them
// cached.
//
// It speaks RESP2 over net and crypto/tls itself — a bounded connection pool,
// AUTH, SELECT, and SET with PX, GET and DEL — rather than through a client
// library, which would bring a dependency tree for five commands.
//
// A Redis outage never fails a request. A lookup that cannot be answered is a
// miss and a write that cannot be made is dropped, each counted in
// gateway_response_cache_store_failures_total. After consecutive failures the
// store stops sending commands for a few seconds and answers at once, so an
// unreachable Redis costs a request no latency; one operation then probes it,
// and the first that succeeds resumes normal service. The change in
// availability is logged once each way, not once per request.
package redisstore

import (
	"crypto/tls"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/ferro-labs/ai-gateway/pkg/cache"
	"github.com/ferro-labs/ai-gateway/pkg/circuitbreaker"
	"github.com/ferro-labs/ai-gateway/pkg/logger"
	"github.com/ferro-labs/ai-gateway/pkg/metrics"
	"github.com/ferro-labs/ai-gateway/providers"
)

const (
	// DefaultTimeout bounds one operation when Options.Timeout is zero.
	DefaultTimeout = 500 * time.Millisecond
	// DefaultPoolSize bounds the open connections when Options.PoolSize is
	// zero.
	DefaultPoolSize = 16

	// failuresToTrip consecutive failed operations mark the store
	// unavailable.
	failuresToTrip = 3
	// unavailableFor is how long an unavailable store answers without sending
	// anything, before one operation probes Redis again.
	unavailableFor = 5 * time.Second
	// warnEvery bounds how often a failure that does not change availability
	// is logged.
	warnEvery = time.Minute

	backendLabel = "redis"

	opGet    = "get"
	opSet    = "set"
	opDelete = "delete"

	reasonError       = "error"
	reasonUnavailable = "unavailable"
	reasonUndecodable = "undecodable"
)

var (
	cmdGET = []byte("GET")
	cmdSET = []byte("SET")
	cmdDEL = []byte("DEL")
	argPX  = []byte("PX")
)

// Options configures a Store.
type Options struct {
	// URL is the redis:// or rediss:// URL to connect to; see ParseURL.
	URL string
	// KeyPrefix is prepended to every key, so several gateways or
	// environments can share one Redis without reading each other's entries.
	KeyPrefix string
	// TTL is how long an entry lives, written as SET ... PX. A TTL under a
	// millisecond stores nothing, since the entry would expire as it landed.
	TTL time.Duration
	// Timeout bounds each operation end to end: waiting for a free
	// connection, dialling one, and the command's round trip. Zero means
	// DefaultTimeout.
	Timeout time.Duration
	// PoolSize bounds the connections open at once, and so the operations in
	// flight. Zero means DefaultPoolSize.
	PoolSize int
	// TLSConfig replaces the default TLS configuration for rediss://, which
	// verifies the server against the system roots. ServerName defaults to
	// the URL's host.
	TLSConfig *tls.Config
}

// Store is a cache.Store backed by Redis. It is safe for concurrent use and
// starts no goroutines.
type Store struct {
	client  *client
	ep      Endpoint
	prefix  string
	px      []byte // the PX argument; nil when nothing is to be stored
	breaker *circuitbreaker.CircuitBreaker

	log      *logger.Logger // nil: logger.Default()
	now      func() time.Time
	cause    atomic.Pointer[error] // the failure that tripped the breaker
	nextWarn atomic.Int64          // unix nanos before which a warning is suppressed
}

var _ cache.Store = (*Store)(nil)

// New builds a Store. It does not connect: the first operation dials, so a
// Redis that is down at startup costs cache misses, not a failed start.
func New(opts Options) (*Store, error) {
	ep, err := ParseURL(opts.URL)
	if err != nil {
		return nil, err
	}
	if opts.Timeout < 0 {
		return nil, errors.New("timeout must not be negative")
	}
	if opts.PoolSize < 0 {
		return nil, errors.New("pool size must not be negative")
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	poolSize := opts.PoolSize
	if poolSize == 0 {
		poolSize = DefaultPoolSize
	}

	var tlsConf *tls.Config
	if ep.TLS {
		if opts.TLSConfig != nil {
			tlsConf = opts.TLSConfig.Clone()
		} else {
			tlsConf = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		if tlsConf.ServerName == "" {
			tlsConf.ServerName = ep.Host
		}
	}

	s := &Store{
		client:  newClient(ep, timeout, poolSize, tlsConf),
		ep:      ep,
		prefix:  opts.KeyPrefix,
		breaker: circuitbreaker.New(failuresToTrip, 1, 1, unavailableFor),
		now:     time.Now,
	}
	if ms := opts.TTL.Milliseconds(); ms > 0 {
		s.px = []byte(strconv.FormatInt(ms, 10))
	}
	s.breaker.SetStateChangeHook(s.availabilityChanged)
	return s, nil
}

// Get returns the response stored under key. A key that is absent, an entry
// this gateway cannot read, and a Redis that cannot be reached are all a miss.
func (s *Store) Get(key string) (*providers.Response, bool) {
	adm, ok := s.breaker.Admit()
	if !ok {
		s.count(opGet, reasonUnavailable)
		return nil, false
	}
	r, err := s.client.do(cmdGET, s.key(key))
	if err == nil && r.kind != '$' {
		err = protocolErrorf("GET answered with a %q reply", r.kind)
	}
	if err != nil {
		s.failed(adm, opGet, err)
		return nil, false
	}
	adm.Success()
	if r.null {
		return nil, false
	}
	resp, err := decodeEntry(r.str)
	if err != nil {
		s.count(opGet, reasonUndecodable)
		s.warn("response-cache: ignoring a redis entry this gateway cannot read; serving a miss", "endpoint", s.ep.String(), "error", err)
		return nil, false
	}
	return resp, true
}

// Set stores resp under key for the store's TTL. A write that cannot be made
// is dropped.
func (s *Store) Set(key string, resp *providers.Response) {
	if s.px == nil || resp == nil {
		return
	}
	value, err := encodeEntry(resp)
	if err != nil {
		s.count(opSet, reasonError)
		s.warn("response-cache: response not cached: it cannot be encoded", "error", err)
		return
	}
	adm, ok := s.breaker.Admit()
	if !ok {
		s.count(opSet, reasonUnavailable)
		return
	}
	r, err := s.client.do(cmdSET, s.key(key), value, argPX, s.px)
	if err == nil {
		err = expectOK(r, nil)
	}
	if err != nil {
		s.failed(adm, opSet, err)
		return
	}
	adm.Success()
}

// Delete removes the entry stored under key, if any.
func (s *Store) Delete(key string) {
	adm, ok := s.breaker.Admit()
	if !ok {
		s.count(opDelete, reasonUnavailable)
		return
	}
	r, err := s.client.do(cmdDEL, s.key(key))
	if err == nil && r.kind != ':' {
		err = protocolErrorf("DEL answered with a %q reply", r.kind)
	}
	if err != nil {
		s.failed(adm, opDelete, err)
		return
	}
	adm.Success()
}

// Close closes the store's connections. An operation after Close is a miss
// or a dropped write.
func (s *Store) Close() error {
	s.client.close()
	return nil
}

func (s *Store) key(key string) []byte {
	return append([]byte(s.prefix), key...)
}

// failed accounts for an operation that was sent and did not complete.
func (s *Store) failed(adm circuitbreaker.Admission, op string, err error) {
	s.count(op, reasonError)
	if errors.Is(err, ErrClosed) {
		// The store's owner closed it; Redis is not to blame.
		adm.Release()
		return
	}
	// Recorded before the breaker hears of it: the transition this failure
	// may cause is logged with it.
	s.cause.Store(&err)
	adm.Failure()
	if s.breaker.State() == circuitbreaker.StateClosed {
		s.warn("response-cache: redis "+op+" failed; "+consequence(op), "endpoint", s.ep.String(), "error", err)
	}
}

func consequence(op string) string {
	if op == opGet {
		return "serving a cache miss"
	}
	return "the write is dropped"
}

// availabilityChanged logs the store becoming unavailable, and available
// again, once each. It runs under the breaker's lock, so it reads only the
// lock-free cause.
func (s *Store) availabilityChanged(from, to circuitbreaker.State) {
	switch {
	case from == circuitbreaker.StateClosed && to == circuitbreaker.StateOpen:
		var cause error
		if p := s.cause.Load(); p != nil {
			cause = *p
		}
		s.logger().Warn("response-cache: redis unavailable; serving cache misses and dropping writes until it answers again",
			"endpoint", s.ep.String(),
			"consecutive_failures", failuresToTrip,
			"retry_every", unavailableFor.String(),
			"error", cause)
	case to == circuitbreaker.StateClosed:
		s.logger().Info("response-cache: redis available again", "endpoint", s.ep.String())
	}
}

// warn logs at most one line per warnEvery, so a failure repeating on every
// request is reported without a line per request.
func (s *Store) warn(msg string, keysAndValues ...any) {
	now := s.now().UnixNano()
	next := s.nextWarn.Load()
	if now < next || !s.nextWarn.CompareAndSwap(next, now+int64(warnEvery)) {
		return
	}
	s.logger().Warn(msg, keysAndValues...)
}

func (s *Store) logger() *logger.Logger {
	if s.log != nil {
		return s.log
	}
	return logger.Default()
}

func (s *Store) count(op, reason string) {
	metrics.ResponseCacheStoreFailures.WithLabelValues(backendLabel, op, reason).Inc()
}

// String names the store's endpoint without its credentials.
func (s *Store) String() string { return fmt.Sprintf("redis store at %s", s.ep) }
