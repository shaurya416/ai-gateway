// Package cache provides a response-cache plugin that stores LLM responses
// and serves them on exact-match cache hits, reducing provider cost and
// latency for repeated requests. Entries live in memory by default; with
// backend: redis they live in a Redis every replica shares. Register it with a
// blank import:
//
//	_ "github.com/ferro-labs/ai-gateway/plugin/cache"
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ferro-labs/ai-gateway/internal/envref"
	cachestore "github.com/ferro-labs/ai-gateway/pkg/cache"
	"github.com/ferro-labs/ai-gateway/pkg/cache/redisstore"
	"github.com/ferro-labs/ai-gateway/pkg/logger"
	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/providers"
)

// metadataCacheBypass marks a request that has no cache key, so neither stage
// caches it — even after a later plugin rewrites it into a form that has one.
const metadataCacheBypass = "response_cache_bypass"

func init() {
	plugin.RegisterFactory("response-cache", func() plugin.Plugin {
		return &ResponseCache{}
	})
}

// ResponseCache is a transform plugin that caches LLM responses using
// exact-match hashing of the request fields that can affect provider output.
type ResponseCache struct {
	// store holds the entries: the in-memory LRU, or a Redis store under
	// backend: redis.
	store cachestore.Store
	// writes is false when nothing may be stored: max_entries 0 on the memory
	// backend, which the operator writes to turn storing off.
	writes bool
	// closer releases what the store holds open — the Redis connections —
	// and is nil for the memory backend.
	closer func() error
}

var _ plugin.ModelPreserving = (*ResponseCache)(nil)

// Name returns the plugin identifier.
func (c *ResponseCache) Name() string {
	return "response-cache"
}

// Type returns the plugin lifecycle hook type.
func (c *ResponseCache) Type() plugin.PluginType {
	return plugin.TypeTransform
}

// The backends a config block can select.
const (
	backendMemory = "memory"
	backendRedis  = "redis"
)

// defaultKeyPrefix namespaces this gateway's entries in a Redis it may share
// with other applications.
const defaultKeyPrefix = "ferrogw:response-cache:"

// settings are the values one response-cache config block resolves to.
type settings struct {
	ttl      time.Duration
	capacity int
	backend  string

	// Read only by backend redis.
	redisURL  string
	keyPrefix string
	timeout   time.Duration
}

// maxAgeSeconds is the longest max_age a time.Duration can carry.
const maxAgeSeconds = math.MaxInt64 / int64(time.Second)

// parseSettings reads and checks a response-cache config block. It is the
// single place the plugin's rules live, shared by Init and ValidateConfig so a
// value the gateway would refuse to start on is the same value `ferrogw
// validate` reports. An absent or null key keeps its default (max_age 300,
// max_entries 1000, backend memory).
//
// A value that is not a whole number of zero or more is a load error. A quoted
// number, or a ${VAR} reference, which resolves to a string, used to be skipped
// in silence and the default served in its place, so a cache configured for a
// minute kept answers for five. A negative max_age, or one under a second,
// expired every entry as it was stored — a cache that loaded, reported itself
// enabled and never served a hit — and a negative or fractional max_entries
// was read as a size other than the one written. Zero is accepted for either
// on the memory backend.
//
// atLoad is true for ValidateConfig. A string setting carrying a ${VAR}
// reference is then passed rather than judged, because it resolves when the
// plugin is constructed and never at load (see plugin.ValidateAction); Init
// judges the resolved value. The checks that depend on the backend are skipped
// with it when the backend itself is a reference.
func parseSettings(config map[string]any, atLoad bool) (settings, error) {
	s := settings{
		ttl:       300 * time.Second,
		capacity:  1000,
		keyPrefix: defaultKeyPrefix,
		timeout:   redisstore.DefaultTimeout,
	}
	if v, ok := config["max_age"]; ok && v != nil {
		n, err := wholeNumber("max_age", v, maxAgeSeconds)
		if err != nil {
			return settings{}, err
		}
		s.ttl = time.Duration(n) * time.Second
	}
	entries := config["max_entries"]
	if entries != nil {
		n, err := wholeNumber("max_entries", entries, math.MaxInt)
		if err != nil {
			return settings{}, err
		}
		s.capacity = int(n)
	}

	backend, err := stringSetting(config["backend"], "backend", atLoad)
	if err != nil {
		return settings{}, err
	}
	redisURL, err := stringSetting(config["redis_url"], "redis_url", atLoad)
	if err != nil {
		return settings{}, err
	}
	// Judged whatever the backend, since a malformed URL is wrong under
	// either. The error never quotes the URL, which can carry a password.
	if redisURL.value != "" && !redisURL.deferred {
		if _, err := redisstore.ParseURL(redisURL.value); err != nil {
			return settings{}, fmt.Errorf("response-cache: redis_url: %w", err)
		}
	}
	prefix, err := stringSetting(config["key_prefix"], "key_prefix", atLoad)
	if err != nil {
		return settings{}, err
	}
	if prefix.set {
		s.keyPrefix = prefix.value
	}
	timeout, err := durationSetting(config["redis_timeout"], "redis_timeout", atLoad)
	if err != nil {
		return settings{}, err
	}
	if timeout > 0 {
		s.timeout = timeout
	}

	if backend.deferred {
		return s, nil
	}
	switch strings.ToLower(strings.TrimSpace(backend.value)) {
	case "", backendMemory:
		s.backend = backendMemory
		// A Redis setting under the memory backend is the mistake this
		// catches: an operator who wrote redis_url and not backend: redis
		// has a cache per replica while believing it shared.
		for _, redisOnly := range []struct {
			key string
			set bool
		}{
			{"redis_url", redisURL.value != ""},
			{"key_prefix", prefix.set},
			{"redis_timeout", config["redis_timeout"] != nil},
		} {
			if redisOnly.set {
				return settings{}, fmt.Errorf("response-cache: %s is read only by backend redis; set backend: redis, or remove it", redisOnly.key)
			}
		}
	case backendRedis:
		s.backend = backendRedis
		if entries != nil {
			return settings{}, errors.New("response-cache: max_entries bounds the memory backend and does not apply to backend redis; size Redis with its own maxmemory and an eviction policy, and remove max_entries")
		}
		if s.ttl <= 0 {
			return settings{}, errors.New("response-cache: backend redis needs max_age of at least 1: Redis refuses an expiry of zero")
		}
		if redisURL.value == "" {
			return settings{}, errors.New("response-cache: backend redis needs redis_url, such as redis://redis:6379/0 or ${REDIS_URL}")
		}
		s.redisURL = redisURL.value
	default:
		return settings{}, fmt.Errorf("response-cache: backend must be %q or %q, got %q", backendMemory, backendRedis, backend.value)
	}
	return s, nil
}

// stringValue is one optional string setting as read.
type stringValue struct {
	value string
	// set is true when the key holds a value rather than being absent or null.
	set bool
	// deferred is true at load for a value carrying a ${VAR} reference, which
	// cannot be judged until the plugin is constructed.
	deferred bool
}

func stringSetting(raw any, key string, atLoad bool) (stringValue, error) {
	v, err := plugin.StringSetting(raw, key)
	if err != nil {
		return stringValue{}, fmt.Errorf("response-cache: %w", err)
	}
	return stringValue{value: v, set: raw != nil, deferred: atLoad && envref.HasReference(v)}, nil
}

// durationSetting reads a Go duration string such as "500ms". Absent, null or
// deferred is zero.
func durationSetting(raw any, key string, atLoad bool) (time.Duration, error) {
	if _, err := plugin.ToFloat64(raw); err == nil {
		return 0, fmt.Errorf("response-cache: %s must be a duration such as 500ms; a bare number has no unit", key)
	}
	v, err := stringSetting(raw, key, atLoad)
	if err != nil || !v.set || v.deferred {
		return 0, err
	}
	d, err := time.ParseDuration(strings.TrimSpace(v.value))
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("response-cache: %s must be a positive duration such as 500ms, got %q", key, v.value)
	}
	return d, nil
}

// wholeNumber converts one configured value and rejects anything that is not a
// whole number from zero up to limit.
func wholeNumber(key string, v any, limit int64) (int64, error) {
	f, err := plugin.ToFloat64(v)
	if err != nil {
		return 0, fmt.Errorf("response-cache: %s: %w", key, err)
	}
	if math.IsNaN(f) || f < 0 || f >= float64(limit) || f != math.Trunc(f) {
		return 0, fmt.Errorf("response-cache: %s must be a whole number >= 0, got %v", key, v)
	}
	return int64(f), nil
}

// ValidateConfig checks the config block without building a cache, so
// `ferrogw validate` and `ferrogw doctor` reject a value this plugin would
// reject at startup. See plugin.ConfigValidator.
func (c *ResponseCache) ValidateConfig(config map[string]any) error {
	_, err := parseSettings(config, true)
	return err
}

// Init configures the plugin from the provided options map. See parseSettings
// for the rules every setting follows.
//
// The redis backend does not connect here. Its first lookup dials, so a Redis
// that is down when the gateway starts or reloads costs cache misses rather
// than a refused config — the same answer an outage later gets.
func (c *ResponseCache) Init(config map[string]any) error {
	s, err := parseSettings(config, false)
	if err != nil {
		return err
	}
	// A second Init releases what the first one opened.
	if err := c.Close(); err != nil {
		return err
	}
	if s.backend == backendRedis {
		store, err := redisstore.New(redisstore.Options{
			URL:       s.redisURL,
			KeyPrefix: s.keyPrefix,
			TTL:       s.ttl,
			Timeout:   s.timeout,
		})
		if err != nil {
			return fmt.Errorf("response-cache: redis_url: %w", err)
		}
		c.store, c.writes, c.closer = store, true, store.Close
		logger.Default().Info("response-cache: entries shared through redis",
			"store", store.String(), "key_prefix", s.keyPrefix, "max_age", s.ttl.String(), "redis_timeout", s.timeout.String())
		return nil
	}
	c.store, c.writes, c.closer = cachestore.NewMemory(s.capacity, s.ttl), s.capacity > 0, nil
	return nil
}

// Execute checks for a cache hit (before request) or stores the response (after
// request) and does maintenance as per LRU policy.
//
// A hit sets SkipProvider, which suppresses the provider call and nothing else:
// every guardrail, rate limit and budget registered behind this plugin still runs
// on the request, and the after_request stage still records it.
func (c *ResponseCache) Execute(_ context.Context, pctx *plugin.Context) error {
	if pctx.Request == nil || c.store == nil {
		return nil
	}

	// Both stages stand down on a non-chat surface. The request there is a
	// PROJECTION of an embeddings input or an image prompt onto the chat request
	// shape (see plugin.MetadataSurface), so it hashes exactly as the chat
	// request asking the same question — while the response projected back
	// carries no Choices at all. Storing one would serve an empty completion to
	// the next chat request with that text, and never call the provider that
	// would have answered it.
	//
	// Nothing is lost by standing down: the gateway drops whatever this plugin
	// leaves on those surfaces, because a providers.Response cannot hold an
	// embedding or an image. Caching them needs a seam that can, not a key.
	if _, ok := pctx.Metadata[plugin.MetadataSurface]; ok {
		return nil
	}

	// A request with no key at an earlier stage stays out of the cache at
	// every later one, even if a plugin has since changed it into a form that
	// has one.
	if bypass, _ := pctx.Metadata[metadataCacheBypass].(bool); bypass {
		return nil
	}

	if pctx.Stage == plugin.StageBeforeRequest {
		key, ok := keyFor(pctx)
		if !ok {
			return nil
		}
		if pctx.Metadata != nil {
			pctx.Metadata[metaLookupKey] = key
		}
		if resp, ok := c.store.Get(key); ok {
			pctx.Response = cloneResponse(resp)
			pctx.SkipProvider = true
			if pctx.Metadata != nil {
				pctx.Metadata["cache_hit"] = true
			}
		}
		return nil
	}

	// The store runs at after_request and nowhere else. on_error runs because
	// the request failed, and it can still carry a response — one an
	// after_request guardrail refused, or the usage-only, choiceless response an
	// MCP tool loop leaves when it fails part-way. Inferring the store stage as
	// "not before_request" filed those, and a cache also listed at on_error
	// answered every identical request from a failure until it expired.
	//
	// A response this plugin just served is already in the cache; re-storing it
	// would refresh a TTL the operator set from the original fetch.
	if pctx.Stage != plugin.StageAfterRequest || pctx.SkipProvider || pctx.Response == nil {
		return nil
	}

	if !c.writes {
		return nil
	}

	// Stored under the key the lookup used, not one computed afresh. A
	// before_request plugin listed after this one may have rewritten the
	// request since — pii-redact under action redact does — and an entry keyed
	// on the rewritten request is one no lookup ever computes, so every request
	// such a plugin touched missed. Computed here only when this request was
	// never looked up, which is a cache listed at after_request alone.
	key, ok := pctx.Metadata[metaLookupKey].(string)
	if !ok {
		if key, ok = keyFor(pctx); !ok {
			return nil
		}
	}

	// Store a private copy: the caller's resp keeps being mutated after this
	// call returns (e.g. Route/RouteStream stamp OverheadMs post-RunAfter), so
	// the cache must not hold onto the same pointer.
	c.store.Set(key, cloneResponse(pctx.Response))
	return nil
}

// keyFor computes the request's key (see requestKey). Metadata carries the
// opaque APIKey.ID, never the bearer secret, and it is populated before the
// before_request stage, so both stages of one request read the same value. A
// request that has no key — a value JSON cannot encode, a non-finite sampling
// value, or bytes that are not UTF-8 in its credential ID or routing metadata —
// is marked to bypass the cache, and ok is false.
func keyFor(pctx *plugin.Context) (key string, ok bool) {
	keyID, _ := pctx.Metadata["api_key"].(string)
	key, err := requestKey(pctx.Request, keyID)
	if err != nil {
		if pctx.Metadata == nil {
			pctx.Metadata = make(map[string]any)
		}
		pctx.Metadata[metadataCacheBypass] = true
		return "", false
	}
	return key, true
}

// metaLookupKey is the Metadata slot holding the key this request was looked
// up under at before_request, so the after_request store files the response
// where that lookup will find it. Per request, like every Metadata entry; the
// key is a hash and carries no request content.
const metaLookupKey = "response-cache.lookup_key"

// PreservesModel declares that this plugin never changes the model a request
// routes to — it reads the request and answers from a store, and rewrites
// nothing. See plugin.ModelPreserving: it reports TypeTransform, and that alone
// used to cost the deployment the pre-plugin admission check, so enabling
// caching let an unroutable model spend a rate-limit token and a budget on its
// way to a 404.
func (c *ResponseCache) PreservesModel() {}

// Close releases the store's connections, if it holds any. A lookup after
// Close is a miss and a store is dropped.
func (c *ResponseCache) Close() error {
	if c.closer == nil {
		return nil
	}
	return c.closer()
}

// cloneResponse returns a shallow copy of resp so each cache hit gets its own
// top-level struct. Route/RouteStream stamp Object/Created/OverheadMs on the
// response returned from a cache hit; Choices/Metadata/Usage are never
// mutated post-hit, so a shallow copy is sufficient to remove the race on a
// cache entry shared across concurrent callers.
func cloneResponse(resp *providers.Response) *providers.Response {
	clone := *resp
	return &clone
}

// requestKey is the key a request is looked up and stored under: the request's
// cachestore.Fingerprint inside a namespace of the credential and the routing
// metadata. An error means the request has no key, and is not cached.
//
// keyID is the opaque APIKey.ID from Context.Metadata, never the bearer
// secret. The store is shared across callers and identical prompts are the
// common case, so content alone let a response primed by one credential be
// served to another — the caller inherits an answer it was never authorised
// to receive, and the guardrails, budget and rate limits that would have run
// against its own call are evaluated against a request that never reaches a
// provider. plugin/ratelimit and plugin/budget scope on the same value. An
// empty keyID — an unauthenticated request — shares one namespace with every
// other unauthenticated request and never with an authenticated one.
//
// The provider is not part of the key: Context.Target is unresolved during
// before_request, when a hit must decide whether to skip the provider. This
// assumes a model name has the same meaning across its routing targets.
// Routing metadata can select a different target, so it is in the namespace.
func requestKey(req *providers.Request, keyID string) (string, error) {
	namespace, err := responseCacheNamespace(keyID, req.RoutingMetadata)
	if err != nil {
		return "", err
	}
	return cachestore.Fingerprint(req, string(namespace))
}

// responseCacheNamespace rejects strings that JSON would normalize, preserving
// the byte-for-byte distinctions used by conditional routing and credential IDs.
// JSON sorts map keys and keeps the ID and metadata unambiguous.
func responseCacheNamespace(keyID string, metadata map[string]string) ([]byte, error) {
	if !utf8.ValidString(keyID) {
		return nil, errors.New("cache namespace: invalid UTF-8 identifier")
	}
	for key, value := range metadata {
		if !utf8.ValidString(key) || !utf8.ValidString(value) {
			return nil, errors.New("cache namespace: invalid UTF-8 routing metadata")
		}
	}
	return json.Marshal(struct {
		KeyID           string            `json:"key_id"`
		RoutingMetadata map[string]string `json:"routing_metadata,omitempty"`
	}{KeyID: keyID, RoutingMetadata: metadata})
}
