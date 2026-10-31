package cache

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/pkg/cache/redisstore"
	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/test/redisstub"
)

func redisConfig(srv *redisstub.Server, extra map[string]any) map[string]any {
	config := map[string]any{"backend": "redis", "redis_url": srv.URL(), "max_age": 60}
	for k, v := range extra {
		config[k] = v
	}
	return config
}

func initRedisCache(t *testing.T, config map[string]any) *ResponseCache {
	t.Helper()
	if err := plugin.ValidateConfigFor("response-cache", config); err != nil {
		t.Fatalf("ValidateConfigFor: %v", err)
	}
	c := initCache(t, config)
	t.Cleanup(func() { _ = c.Close() })
	if _, ok := c.store.(*redisstore.Store); !ok {
		t.Fatalf("backend redis built a %T", c.store)
	}
	return c
}

func TestResponseCache_RedisBackend_HitMissAndExpiry(t *testing.T) {
	t.Parallel()

	srv := redisstub.New(t)
	c := initRedisCache(t, redisConfig(srv, map[string]any{"max_age": 30}))
	req := testRequest("gpt-4", "hello")

	if lookupAs(t, c, req, "key-a") {
		t.Fatal("hit on an empty Redis")
	}
	want := testResponse()
	storeAs(t, c, req, "key-a", want)

	pctx := plugin.NewContext(req)
	pctx.Stage = plugin.StageBeforeRequest
	pctx.Metadata["api_key"] = "key-a"
	if err := c.Execute(context.Background(), pctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !pctx.SkipProvider || pctx.Metadata["cache_hit"] != true {
		t.Fatal("miss after the response was stored")
	}
	if !reflect.DeepEqual(pctx.Response, want) {
		t.Fatalf("served %#v, want %#v", pctx.Response, want)
	}

	// One entry, under the default prefix, expiring with max_age.
	keys := srv.Keys(0)
	if len(keys) != 1 || !strings.HasPrefix(keys[0], defaultKeyPrefix) {
		t.Fatalf("keys = %v, want one under %q", keys, defaultKeyPrefix)
	}
	if px, _ := srv.PX(0, keys[0]); px != 30_000 {
		t.Fatalf("PX = %d, want 30000", px)
	}

	// The credential still scopes the entry.
	if lookupAs(t, c, req, "key-b") {
		t.Fatal("one credential's entry served another")
	}

	srv.Advance(30 * time.Second)
	if lookupAs(t, c, req, "key-a") {
		t.Fatal("hit after max_age elapsed")
	}
}

// The point of the backend: replicas pointed at one Redis serve each other's
// entries, and a different key_prefix is a different cache.
func TestResponseCache_RedisBackend_InstancesShareOneRedis(t *testing.T) {
	t.Parallel()

	srv := redisstub.New(t)
	replicaA := initRedisCache(t, redisConfig(srv, nil))
	replicaB := initRedisCache(t, redisConfig(srv, nil))
	staging := initRedisCache(t, redisConfig(srv, map[string]any{"key_prefix": "staging:"}))
	req := testRequest("gpt-4", "hello")

	storeAs(t, replicaA, req, "key-a", testResponse())
	if !lookupAs(t, replicaB, req, "key-a") {
		t.Fatal("a second replica missed the entry the first one stored")
	}
	if lookupAs(t, staging, req, "key-a") {
		t.Fatal("another key_prefix served the entry")
	}
}

// A Redis outage is a miss at before_request and a dropped write at
// after_request — never an error. The plugin is a transform, which fails
// closed: an error here would answer 500 to every request while Redis is down.
func TestResponseCache_RedisBackend_OutageIsAMissNotAnError(t *testing.T) {
	t.Parallel()

	srv := redisstub.New(t)
	c := initRedisCache(t, redisConfig(srv, nil))
	req := testRequest("gpt-4", "hello")
	storeAs(t, c, req, "", testResponse())
	srv.Stop()

	m := plugin.NewManager(nil)
	for _, stage := range []plugin.Stage{plugin.StageBeforeRequest, plugin.StageAfterRequest} {
		if err := m.Register(stage, c); err != nil {
			t.Fatalf("Register at %s: %v", stage, err)
		}
	}
	pctx := plugin.NewContext(req)
	if err := m.RunBefore(context.Background(), pctx); err != nil {
		t.Fatalf("RunBefore while Redis is down = %v, want a miss", err)
	}
	if pctx.SkipProvider || pctx.Response != nil {
		t.Fatal("served a response while Redis was down")
	}
	pctx.Response = testResponse()
	if err := m.RunAfter(context.Background(), pctx); err != nil {
		t.Fatalf("RunAfter while Redis is down = %v, want the write dropped", err)
	}

	// Two failures do not mark the store unavailable, so the first lookup
	// after Redis returns is served from it again.
	srv.Restart()
	if !lookupAs(t, c, req, "") {
		t.Fatal("miss once Redis was back; the entry survived")
	}
}

// Each of these is refused by Init and by ferrogw validate alike, and none of
// the errors quotes the credential.
func TestResponseCache_RedisSettingsAreValidated(t *testing.T) {
	t.Parallel()

	const url = "redis://:s3cret@cache:6379/0"
	for _, tc := range []struct {
		name   string
		config map[string]any
		want   string
	}{
		{"unknown backend", map[string]any{"backend": "memcached"}, `backend must be "memory" or "redis"`},
		{"backend not a string", map[string]any{"backend": 1}, "backend must be a string"},
		{"redis without a URL", map[string]any{"backend": "redis"}, "needs redis_url"},
		{"redis with an empty URL", map[string]any{"backend": "redis", "redis_url": ""}, "needs redis_url"},
		{"URL with a query string", map[string]any{"backend": "redis", "redis_url": url + "?tls=true"}, "query string"},
		{"URL with another scheme", map[string]any{"backend": "redis", "redis_url": "https://:s3cret@cache"}, "redis:// or rediss://"},
		{"URL with a username and no password", map[string]any{"backend": "redis", "redis_url": "redis://s3cret@cache"}, "no password"},
		{"URL that does not parse", map[string]any{"backend": "redis", "redis_url": "redis://:s3cret/x@cache"}, "percent-encode"},
		{"URL not a string", map[string]any{"backend": "redis", "redis_url": 6379}, "redis_url must be a string"},
		{"max_entries under redis", map[string]any{"backend": "redis", "redis_url": url, "max_entries": 1000}, "max_entries bounds the memory backend"},
		{"max_age zero under redis", map[string]any{"backend": "redis", "redis_url": url, "max_age": 0}, "max_age of at least 1"},
		{"redis_url under memory", map[string]any{"redis_url": url}, "redis_url is read only by backend redis"},
		{"key_prefix under memory", map[string]any{"backend": "memory", "key_prefix": "x:"}, "key_prefix is read only by backend redis"},
		{"redis_timeout under memory", map[string]any{"redis_timeout": "1s"}, "redis_timeout is read only by backend redis"},
		{"timeout without a unit", map[string]any{"backend": "redis", "redis_url": url, "redis_timeout": 500}, "no unit"},
		{"timeout that does not parse", map[string]any{"backend": "redis", "redis_url": url, "redis_timeout": "soon"}, "positive duration"},
		{"negative timeout", map[string]any{"backend": "redis", "redis_url": url, "redis_timeout": "-1s"}, "positive duration"},
		{"key_prefix not a string", map[string]any{"backend": "redis", "redis_url": url, "key_prefix": 7}, "key_prefix must be a string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := (&ResponseCache{}).Init(tc.config)
			if err == nil {
				t.Fatalf("Init accepted %v", tc.config)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Init error %q does not say %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "s3cret") {
				t.Errorf("Init error %q quotes the credential", err)
			}
			if verr := plugin.ValidateConfigFor("response-cache", tc.config); verr == nil || verr.Error() != err.Error() {
				t.Errorf("ValidateConfigFor = %v, want the error Init gives", verr)
			}
		})
	}
}

// A ${VAR} reference resolves when the plugin is constructed, never at load,
// so validate passes it rather than judging the reference text — and judges
// the rest of the block as usual.
func TestResponseCache_RedisReferencesAreJudgedAtConstruction(t *testing.T) {
	t.Parallel()

	for _, config := range []map[string]any{
		{"backend": "redis", "redis_url": "${REDIS_URL}"},
		{"backend": "redis", "redis_url": "redis://:${REDIS_PASSWORD}@redis:6379/0"},
		{"backend": "redis", "redis_url": "${REDIS_URL}", "key_prefix": "${ENVIRONMENT}:", "redis_timeout": "${REDIS_TIMEOUT}"},
		// The backend itself named by the environment: nothing that depends
		// on it can be judged yet.
		{"backend": "${CACHE_BACKEND}", "redis_url": "${REDIS_URL}", "max_entries": 500},
	} {
		if err := plugin.ValidateConfigFor("response-cache", config); err != nil {
			t.Errorf("ValidateConfigFor(%v) = %v, want the references passed", config, err)
		}
	}

	if err := plugin.ValidateConfigFor("response-cache", map[string]any{
		"backend": "redis", "redis_url": "${REDIS_URL}", "max_entries": 500,
	}); err == nil {
		t.Error("a reference in redis_url excused max_entries under backend redis")
	}

	// Init sees what construction hands it. A reference that reaches it
	// unresolved — expansion turned off — is read as the literal it is.
	if err := (&ResponseCache{}).Init(map[string]any{"backend": "redis", "redis_url": "${REDIS_URL}"}); err == nil {
		t.Error("Init accepted an unresolved reference as a URL")
	}
}

func TestResponseCache_RedisBackend_AcceptsEveryRedisSetting(t *testing.T) {
	t.Parallel()

	srv := redisstub.New(t)
	srv.RequireAuth("gateway", "s3cret")
	c := initRedisCache(t, map[string]any{
		"backend":       "Redis",
		"redis_url":     "redis://gateway:s3cret@" + srv.Addr() + "/2",
		"key_prefix":    "prod:",
		"redis_timeout": "250ms",
		"max_age":       120,
	})
	req := testRequest("gpt-4", "hello")
	storeAs(t, c, req, "", testResponse())
	if !lookupAs(t, c, req, "") {
		t.Fatal("miss after a store")
	}
	keys := srv.Keys(2)
	if len(keys) != 1 || !strings.HasPrefix(keys[0], "prod:") {
		t.Fatalf("database 2 holds %v, want one key under prod:", keys)
	}
}

// Close releases the connections; a re-Init releases the store the previous
// Init built.
func TestResponseCache_RedisBackend_CloseReleasesTheStore(t *testing.T) {
	t.Parallel()

	srv := redisstub.New(t)
	c := initRedisCache(t, redisConfig(srv, nil))
	first := c.store.(*redisstore.Store)
	req := testRequest("gpt-4", "hello")
	storeAs(t, c, req, "", testResponse())

	if err := c.Init(map[string]any{}); err != nil {
		t.Fatalf("re-Init: %v", err)
	}
	if _, ok := first.Get(cacheKey(req, "")); ok {
		t.Fatal("the store a re-Init replaced still served a hit")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := errors.Join(c.Close(), first.Close()); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
}
