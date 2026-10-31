package redisstore

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"go.uber.org/goleak"

	"github.com/ferro-labs/ai-gateway/pkg/logger"
	"github.com/ferro-labs/ai-gateway/pkg/metrics"
	"github.com/ferro-labs/ai-gateway/providers"
	"github.com/ferro-labs/ai-gateway/test/redisstub"
)

// The store starts no goroutines, and the stub waits for its own: anything
// left running when the tests end is a connection or a timer the store leaked.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

const testPrefix = "test:"

func newStore(t *testing.T, opts Options) *Store {
	t.Helper()
	if opts.KeyPrefix == "" {
		opts.KeyPrefix = testPrefix
	}
	if opts.TTL == 0 {
		opts.TTL = time.Minute
	}
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func sampleResponse(id string) *providers.Response {
	return &providers.Response{
		ID:       id,
		Model:    "gpt-4o",
		Provider: "openai",
		Choices: []providers.Choice{{
			Message:      providers.Message{Role: "assistant", Content: "hello"},
			FinishReason: "stop",
		}},
		Usage: providers.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8},
	}
}

func failures(t *testing.T, op, reason string) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := metrics.ResponseCacheStoreFailures.WithLabelValues(backendLabel, op, reason).Write(m); err != nil {
		t.Fatalf("read failure counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

// syncBuffer collects log lines written from any goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// lines returns the logged lines at level whose message contains substr.
func (b *syncBuffer) lines(t *testing.T, level, substr string) []string {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if rec["level"] == level && strings.Contains(rec["msg"].(string), substr) {
			out = append(out, line)
		}
	}
	return out
}

func captureLog(s *Store) *syncBuffer {
	buf := &syncBuffer{}
	s.log = logger.New(logger.Options{Level: "debug", Output: buf})
	return buf
}

func TestStore_SetThenGet(t *testing.T) {
	t.Parallel()

	srv := redisstub.New(t)
	s := newStore(t, Options{URL: srv.URL(), TTL: 90 * time.Second})

	if _, ok := s.Get("k"); ok {
		t.Fatal("hit on an empty Redis")
	}
	want := sampleResponse("resp-1")
	s.Set("k", want)

	got, ok := s.Get("k")
	if !ok {
		t.Fatal("miss after Set")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Get = %#v, want %#v", got, want)
	}

	// Stored under the prefix, with the TTL as PX.
	if keys := srv.Keys(0); !slices.Equal(keys, []string{testPrefix + "k"}) {
		t.Fatalf("keys = %v, want [%sk]", keys, testPrefix)
	}
	if px, _ := srv.PX(0, testPrefix+"k"); px != 90_000 {
		t.Fatalf("PX = %d, want 90000", px)
	}
}

func TestStore_EntryExpiresWithItsTTL(t *testing.T) {
	t.Parallel()

	srv := redisstub.New(t)
	s := newStore(t, Options{URL: srv.URL(), TTL: 10 * time.Second})

	s.Set("k", sampleResponse("resp-1"))
	srv.Advance(9 * time.Second)
	if _, ok := s.Get("k"); !ok {
		t.Fatal("miss before the TTL elapsed")
	}
	srv.Advance(time.Second)
	if _, ok := s.Get("k"); ok {
		t.Fatal("hit after the TTL elapsed")
	}
}

func TestStore_DeleteRemovesTheEntry(t *testing.T) {
	t.Parallel()

	srv := redisstub.New(t)
	s := newStore(t, Options{URL: srv.URL()})

	s.Set("k", sampleResponse("resp-1"))
	s.Delete("k")
	if _, ok := s.Get("k"); ok {
		t.Fatal("hit after Delete")
	}
}

// Two stores with different prefixes on one Redis are two caches.
func TestStore_PrefixSeparatesStores(t *testing.T) {
	t.Parallel()

	srv := redisstub.New(t)
	staging := newStore(t, Options{URL: srv.URL(), KeyPrefix: "staging:"})
	production := newStore(t, Options{URL: srv.URL(), KeyPrefix: "production:"})

	staging.Set("k", sampleResponse("staging"))
	if _, ok := production.Get("k"); ok {
		t.Fatal("one prefix served another's entry")
	}
	if got, ok := staging.Get("k"); !ok || got.ID != "staging" {
		t.Fatalf("staging.Get = %v, %v", got, ok)
	}
}

func TestStore_ZeroTTLStoresNothing(t *testing.T) {
	t.Parallel()

	srv := redisstub.New(t)
	s, err := New(Options{URL: srv.URL(), KeyPrefix: testPrefix})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	s.Set("k", sampleResponse("resp-1"))
	if cmds := srv.Commands(); slices.Contains(cmds, "SET") {
		t.Fatalf("a zero TTL sent SET (%v); the entry would expire as it landed", cmds)
	}
}

func TestStore_AuthenticatesWithPasswordAlone(t *testing.T) {
	t.Parallel()

	srv := redisstub.New(t)
	srv.RequireAuth("", "s3cret")
	s := newStore(t, Options{URL: "redis://:s3cret@" + srv.Addr()})

	s.Set("k", sampleResponse("resp-1"))
	if _, ok := s.Get("k"); !ok {
		t.Fatal("miss after an authenticated Set")
	}
	// Once per connection, not once per command.
	if got := srv.Commands(); !slices.Equal(got, []string{"AUTH", "SET", "GET"}) {
		t.Fatalf("commands = %v, want [AUTH SET GET]", got)
	}
}

func TestStore_AuthenticatesWithUsernameAndPassword(t *testing.T) {
	t.Parallel()

	srv := redisstub.New(t)
	srv.RequireAuth("gateway", "s3cret")
	s := newStore(t, Options{URL: "redis://gateway:s3cret@" + srv.Addr()})

	s.Set("k", sampleResponse("resp-1"))
	if _, ok := s.Get("k"); !ok {
		t.Fatal("miss after an authenticated Set")
	}
}

func TestStore_SelectsTheDatabaseInThePath(t *testing.T) {
	t.Parallel()

	srv := redisstub.New(t)
	s := newStore(t, Options{URL: srv.URL() + "/3"})

	s.Set("k", sampleResponse("resp-1"))
	if keys := srv.Keys(3); len(keys) != 1 {
		t.Fatalf("database 3 holds %v, want the one entry", keys)
	}
	if keys := srv.Keys(0); len(keys) != 0 {
		t.Fatalf("database 0 holds %v, want nothing", keys)
	}
}

func TestStore_TLS(t *testing.T) {
	t.Parallel()

	srv := redisstub.NewTLS(t)
	s := newStore(t, Options{
		URL:       srv.URL(),
		TLSConfig: &tls.Config{RootCAs: srv.RootCAs(), MinVersion: tls.VersionTLS12},
	})
	s.Set("k", sampleResponse("resp-1"))
	if _, ok := s.Get("k"); !ok {
		t.Fatal("miss over TLS")
	}
}

// The certificate is verified: a server the client cannot vouch for is not
// one it sends a password to.
func TestStore_TLSRefusesAnUntrustedCertificate(t *testing.T) {
	srv := redisstub.NewTLS(t)
	s := newStore(t, Options{URL: srv.URL()})
	captureLog(s)

	before := failures(t, opSet, reasonError)
	s.Set("k", sampleResponse("resp-1"))
	if got := failures(t, opSet, reasonError) - before; got != 1 {
		t.Fatalf("set failures = %v, want 1", got)
	}
	if keys := srv.Keys(0); len(keys) != 0 {
		t.Fatalf("an unverified server received %v", keys)
	}
}

// A wrong password is a failed operation — a miss — and is never quoted.
func TestStore_RefusedAuthIsAMissThatDoesNotQuoteThePassword(t *testing.T) {
	srv := redisstub.New(t)
	srv.RequireAuth("", "right")
	s := newStore(t, Options{URL: "redis://:wr0ng-s3cret@" + srv.Addr()})
	logs := captureLog(s)

	before := failures(t, opGet, reasonError)
	if _, ok := s.Get("k"); ok {
		t.Fatal("hit with a refused AUTH")
	}
	if got := failures(t, opGet, reasonError) - before; got != 1 {
		t.Fatalf("get failures = %v, want 1", got)
	}
	warned := logs.lines(t, "WARN", "redis get failed")
	if len(warned) != 1 || !strings.Contains(warned[0], "WRONGPASS") {
		t.Fatalf("warnings = %v, want one naming WRONGPASS", warned)
	}
	if strings.Contains(warned[0], "wr0ng-s3cret") {
		t.Fatalf("warning quotes the password: %s", warned[0])
	}
}

// An error reply is Redis answering: the operation fails, and the connection
// it came back on is still in step and is reused.
func TestStore_ErrorReplyFailsTheOperationAndKeepsTheConnection(t *testing.T) {
	srv := redisstub.New(t)
	s := newStore(t, Options{URL: srv.URL()})
	captureLog(s)

	srv.FailCommand("SET", "OOM command not allowed when used memory > 'maxmemory'.")
	before := failures(t, opSet, reasonError)
	s.Set("k", sampleResponse("resp-1"))
	if got := failures(t, opSet, reasonError) - before; got != 1 {
		t.Fatalf("set failures = %v, want 1", got)
	}
	srv.FailCommand("SET", "")

	s.Set("k", sampleResponse("resp-1"))
	if _, ok := s.Get("k"); !ok {
		t.Fatal("miss after the error reply cleared")
	}
	if n := srv.Accepted(); n != 1 {
		t.Fatalf("accepted %d connections, want 1: an error reply must not cost the connection", n)
	}
}

// A connection that drops partway through a reply is out of step and is not
// reused; the next operation dials afresh and succeeds. The reply here is cut
// on a connection dialled for it, which is not retried: only a connection that
// sat in the pool can have died without the server seeing the command.
func TestStore_ConnectionDroppedMidReply(t *testing.T) {
	srv := redisstub.New(t)
	s := newStore(t, Options{URL: srv.URL()})
	captureLog(s)

	value, err := encodeEntry(sampleResponse("resp-1"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Put(0, testPrefix+"k", value)
	srv.TruncateReplies(1)

	before := failures(t, opGet, reasonError)
	if _, ok := s.Get("k"); ok {
		t.Fatal("hit from a reply cut short")
	}
	if got := failures(t, opGet, reasonError) - before; got != 1 {
		t.Fatalf("get failures = %v, want 1", got)
	}
	if _, ok := s.Get("k"); !ok {
		t.Fatal("miss after the connection was replaced")
	}
	if n := srv.Accepted(); n != 2 {
		t.Fatalf("accepted %d connections, want 2: the cut connection must not be reused", n)
	}
}

// A pooled connection the server closed while it sat idle — a Redis restart,
// an idle timeout — is replaced inside the same operation, so the caller sees
// a hit rather than a failure.
func TestStore_StalePooledConnectionIsReplacedTransparently(t *testing.T) {
	srv := redisstub.New(t)
	s := newStore(t, Options{URL: srv.URL()})
	captureLog(s)

	s.Set("k", sampleResponse("resp-1"))
	srv.DropConnections()

	before := failures(t, opGet, reasonError)
	if _, ok := s.Get("k"); !ok {
		t.Fatal("miss after the server closed the pooled connection")
	}
	if got := failures(t, opGet, reasonError) - before; got != 0 {
		t.Fatalf("get failures = %v, want 0", got)
	}
	if n := srv.Accepted(); n != 2 {
		t.Fatalf("accepted %d connections, want 2", n)
	}
}

// A slow Redis costs a request the timeout and no more.
func TestStore_TimeoutBoundsAnOperation(t *testing.T) {
	srv := redisstub.New(t)
	s := newStore(t, Options{URL: srv.URL(), Timeout: 50 * time.Millisecond})
	captureLog(s)

	s.Set("k", sampleResponse("resp-1"))
	srv.Stall(2 * time.Second)

	before := failures(t, opGet, reasonError)
	start := time.Now()
	if _, ok := s.Get("k"); ok {
		t.Fatal("hit from a stalled Redis")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Get took %v against a 50ms timeout", elapsed)
	}
	if got := failures(t, opGet, reasonError) - before; got != 1 {
		t.Fatalf("get failures = %v, want 1", got)
	}
}

// The pool bounds open connections: more concurrent operations than slots wait
// for one rather than dialling more.
func TestStore_PoolBoundsOpenConnections(t *testing.T) {
	t.Parallel()

	srv := redisstub.New(t)
	s := newStore(t, Options{URL: srv.URL(), PoolSize: 2, Timeout: 5 * time.Second})
	srv.Stall(10 * time.Millisecond)

	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() { s.Get("k") })
	}
	wg.Wait()
	if n := srv.Accepted(); n > 2 {
		t.Fatalf("accepted %d connections with a pool of 2", n)
	}
}

// An outage is logged once as it starts and once as it ends. Between the two,
// operations do not reach Redis at all — they are answered at once — and are
// counted as unavailable.
func TestStore_OutageIsLoggedOncePerTransition(t *testing.T) {
	srv := redisstub.New(t)
	s := newStore(t, Options{URL: srv.URL()})
	logs := captureLog(s)
	clock := time.Now()
	s.breaker.SetNowForTest(func() time.Time { return clock })

	s.Set("k", sampleResponse("resp-1"))
	srv.Stop()

	unavailableBefore := failures(t, opGet, reasonUnavailable)
	for range 50 {
		if _, ok := s.Get("k"); ok {
			t.Fatal("hit while Redis was down")
		}
	}
	if got := failures(t, opGet, reasonUnavailable) - unavailableBefore; got != 50-failuresToTrip {
		t.Fatalf("unavailable = %v, want %d: every operation after the trip must be answered without Redis", got, 50-failuresToTrip)
	}
	if got := logs.lines(t, "WARN", "redis unavailable"); len(got) != 1 {
		t.Fatalf("unavailable warnings = %d, want 1:\n%s", len(got), strings.Join(got, "\n"))
	}
	if got := logs.lines(t, "WARN", "redis get failed"); len(got) > 1 {
		t.Fatalf("per-failure warnings = %d, want at most 1:\n%s", len(got), strings.Join(got, "\n"))
	}

	// Still down when the probe goes: back to unavailable, and no new line.
	clock = clock.Add(unavailableFor + time.Millisecond)
	s.Get("k")
	if got := logs.lines(t, "WARN", "redis unavailable"); len(got) != 1 {
		t.Fatalf("a failed probe logged again: %d unavailable warnings", len(got))
	}

	srv.Restart()
	clock = clock.Add(unavailableFor + time.Millisecond)
	if _, ok := s.Get("k"); !ok {
		t.Fatal("miss once Redis was back; the entry survived the restart")
	}
	if got := logs.lines(t, "INFO", "redis available again"); len(got) != 1 {
		t.Fatalf("recovery lines = %d, want 1", len(got))
	}
	s.Set("k2", sampleResponse("resp-2"))
	if _, ok := s.Get("k2"); !ok {
		t.Fatal("miss after recovery")
	}
}

// An entry this gateway cannot read — another writer under the same prefix, or
// a later format — is a miss, not a request failure and not an outage.
func TestStore_UndecodableEntryIsAMiss(t *testing.T) {
	srv := redisstub.New(t)
	s := newStore(t, Options{URL: srv.URL()})
	logs := captureLog(s)

	srv.Put(0, testPrefix+"other", []byte("written by something else"))
	srv.Put(0, testPrefix+"future", []byte(`{"v":99,"response":{}}`))

	before := failures(t, opGet, reasonUndecodable)
	for range 5 {
		if _, ok := s.Get("other"); ok {
			t.Fatal("hit on a foreign value")
		}
		if _, ok := s.Get("future"); ok {
			t.Fatal("hit on a later format")
		}
	}
	if got := failures(t, opGet, reasonUndecodable) - before; got != 10 {
		t.Fatalf("undecodable = %v, want 10", got)
	}
	if got := logs.lines(t, "WARN", "cannot read"); len(got) != 1 {
		t.Fatalf("undecodable warnings = %d, want 1 (rate-limited)", len(got))
	}
	if s.breaker.State().String() != "closed" {
		t.Fatalf("breaker %s: an unreadable entry is not an outage", s.breaker.State())
	}
}

func TestStore_ClosedStoreClosesItsConnections(t *testing.T) {
	srv := redisstub.New(t)
	s, err := New(Options{URL: srv.URL(), KeyPrefix: testPrefix, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	captureLog(s)
	s.Set("k", sampleResponse("resp-1"))
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok := s.Get("k"); ok {
		t.Fatal("hit from a closed store")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if s.breaker.State().String() != "closed" {
		t.Fatalf("breaker %s: closing the store is not a Redis failure", s.breaker.State())
	}
}

// An entry larger than the reply parser reads back is not written. Stored, it
// would make every lookup of its key a protocol error counted against Redis,
// and a request repeated often enough would mark the store unavailable for
// every key.
func TestStore_EntryTooLargeToReadBackIsNotStored(t *testing.T) {
	srv := redisstub.New(t)
	s := newStore(t, Options{URL: srv.URL()})
	logs := captureLog(s)
	if s.maxValue != maxBulk {
		t.Fatalf("maxValue = %d, want maxBulk (%d), the bound the reply parser enforces", s.maxValue, maxBulk)
	}

	value, err := encodeEntry(sampleResponse("resp-1"))
	if err != nil {
		t.Fatal(err)
	}
	s.maxValue = len(value) - 1

	before := failures(t, opSet, reasonError)
	s.Set("k", sampleResponse("resp-1"))
	if got := failures(t, opSet, reasonError) - before; got != 1 {
		t.Fatalf("set failures = %v, want 1", got)
	}
	if slices.Contains(srv.Commands(), "SET") {
		t.Fatal("an entry too large to read back was sent")
	}
	if got := logs.lines(t, "WARN", "larger than the store reads back"); len(got) != 1 {
		t.Fatalf("oversize warnings = %d, want 1", len(got))
	}

	s.maxValue = len(value)
	s.Set("k", sampleResponse("resp-1"))
	if _, ok := s.Get("k"); !ok {
		t.Fatal("miss for an entry exactly at the bound")
	}
}

func TestNew_RefusesABadURL(t *testing.T) {
	t.Parallel()

	if _, err := New(Options{URL: "redis://:s3cret@cache?tls=true"}); err == nil {
		t.Fatal("New accepted a URL carrying a query string")
	}
	if _, err := New(Options{URL: "redis://cache", Timeout: -time.Second}); err == nil {
		t.Fatal("New accepted a negative timeout")
	}
}
