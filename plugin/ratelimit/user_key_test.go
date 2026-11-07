package ratelimit

import (
	"context"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/providers/core"
)

// executeAsUser runs one request carrying user through p and reports whether it
// was admitted.
func executeAsUser(t *testing.T, p *Plugin, user string) bool {
	t.Helper()
	pctx := &plugin.Context{Request: &core.Request{User: user}, Metadata: map[string]any{}}
	if err := p.Execute(context.Background(), pctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return !pctx.Reject
}

func liveHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// The per-user bucket store caps how many users it tracks, and the cap is what
// stands between a caller and the gateway's memory. The user is the request
// body's own `user` field, bounded only by the request size, so a bucket keyed
// on the raw value kept a copy of whatever the caller sent: 64 users of 1 MiB
// each held 64 MiB after every one of those requests had finished, and the
// 100,000-bucket cap bounded the number of buckets rather than the memory
// behind them.
func TestPlugin_UserBucketsDoNotRetainTheUserValue(t *testing.T) {
	const (
		users    = 64
		userSize = 1 << 20
		maxHeld  = 16 << 20
	)
	p := newPlugin(t, map[string]any{"requests_per_second": 1e6, "burst": 1e6, "user_rpm": 60.0})

	before := liveHeap()
	for i := range users {
		user := strconv.Itoa(i) + strings.Repeat("u", userSize)
		if !executeAsUser(t, p, user) {
			t.Fatalf("first request from user %d was refused", i)
		}
	}
	after := liveHeap()
	runtime.KeepAlive(p)

	if after > before && after-before > maxHeld {
		t.Fatalf("%d finished requests left %d MiB live in the per-user buckets, want under %d MiB",
			users, (after-before)>>20, maxHeld>>20)
	}
}

// A user too long to be worth keeping is still a user: the same long value is
// held to its own rate, and a different one that shares all but its last byte
// has a bucket of its own.
func TestPlugin_LongUserKeepsItsOwnBucket(t *testing.T) {
	p := newPlugin(t, map[string]any{"requests_per_second": 1e6, "burst": 1e6, "user_rpm": 1.0})
	long := strings.Repeat("u", 1<<16)

	if !executeAsUser(t, p, long+"a") {
		t.Fatal("first request from a long user was refused")
	}
	if executeAsUser(t, p, long+"a") {
		t.Fatal("second request from the same long user was admitted under user_rpm: 1")
	}
	if !executeAsUser(t, p, long+"b") {
		t.Fatal("a different long user was refused on the first user's bucket")
	}
}
