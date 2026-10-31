package cache

import "github.com/ferro-labs/ai-gateway/providers"

// Store is what the response-cache plugin reads cached responses from and
// writes them to. Memory is the default.
//
// Neither method reports an error, and that is the contract rather than an
// omission. The plugin is a transform, and a transform that breaks fails the
// request closed, so a store able to fail a lookup would fail every request
// that reached it. A store that cannot answer reports a miss from Get, drops
// the write in Set, and accounts for the failure itself.
//
// Get may return a response shared with other callers, so it must not be
// mutated; the plugin copies it before handing it on. Set takes ownership of
// resp.
type Store interface {
	Get(key string) (*providers.Response, bool)
	Set(key string, resp *providers.Response)
}

var _ Store = (*Memory)(nil)
