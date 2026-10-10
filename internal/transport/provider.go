package transport

import (
	"time"
)

// ProviderPreset contains tuned transport settings for a known provider.
// These are based on observed provider behaviour in production.
type ProviderPreset struct {
	// MaxIdleConnsPerHost controls the idle connection pool size.
	// High-traffic providers (OpenAI, Anthropic) benefit from larger pools.
	MaxIdleConnsPerHost int

	// ResponseHeaderTimeout is the maximum time to wait for response headers.
	// Some providers (Bedrock, Vertex) have higher cold-start latency. It only
	// ever raises the base config's bound — see applyPreset.
	ResponseHeaderTimeout time.Duration

	// DialTimeout overrides the default dial timeout.
	DialTimeout time.Duration
}

// KnownProviderPresets returns tuned transport settings for high-traffic
// providers. Providers not in this map use DefaultConfig().
//
// These presets are informed by production traffic patterns:
//   - OpenAI/Azure: highest traffic, needs largest pools
//   - Anthropic: large prompts → higher header timeout
//   - Bedrock/Vertex: cloud-native cold starts → higher dial+header timeout
//   - Ollama: local, low latency → smaller pools
func KnownProviderPresets() map[string]ProviderPreset {
	return map[string]ProviderPreset{
		"openai": {
			MaxIdleConnsPerHost: 200,
		},
		"azure-openai": {
			MaxIdleConnsPerHost: 200,
		},
		"anthropic": {
			MaxIdleConnsPerHost:   150,
			ResponseHeaderTimeout: 60 * time.Second,
		},
		"gemini": {
			MaxIdleConnsPerHost: 100,
		},
		"bedrock": {
			MaxIdleConnsPerHost:   100,
			ResponseHeaderTimeout: 120 * time.Second,
			DialTimeout:           15 * time.Second,
		},
		"vertex-ai": {
			MaxIdleConnsPerHost:   100,
			ResponseHeaderTimeout: 120 * time.Second,
			DialTimeout:           15 * time.Second,
		},
		"databricks": {
			MaxIdleConnsPerHost:   100,
			ResponseHeaderTimeout: 120 * time.Second,
			DialTimeout:           15 * time.Second,
		},
		"groq": {
			MaxIdleConnsPerHost: 100,
		},
		"ollama": {
			MaxIdleConnsPerHost:   20,
			ResponseHeaderTimeout: 120 * time.Second,
			DialTimeout:           5 * time.Second,
		},
		"replicate": {
			// Replicate's async prediction API holds the connection open for
			// "Prefer: wait" submissions (~60s); the default 30s header timeout
			// would abort these before the prediction resolves.
			MaxIdleConnsPerHost:   100,
			ResponseHeaderTimeout: 65 * time.Second,
		},
		"azure-foundry": {
			// Another Azure OpenAI-wire endpoint; mirror azure-openai.
			MaxIdleConnsPerHost: 200,
		},
		"ollama-cloud": {
			// Serves large 120b/480b/671b models; the default 30s header
			// timeout can abort a slow first response, as for local ollama.
			MaxIdleConnsPerHost:   100,
			ResponseHeaderTimeout: 120 * time.Second,
			DialTimeout:           10 * time.Second,
		},
		"perplexity": {
			// sonar-deep-research can run many web searches before emitting the
			// first response header; raise the timeout to avoid a 30s abort.
			MaxIdleConnsPerHost:   100,
			ResponseHeaderTimeout: 65 * time.Second,
		},
	}
}

// applyPreset merges a ProviderPreset into a base Config.
// Zero-valued preset fields are left at the base config defaults.
//
// A preset's ResponseHeaderTimeout raises the base bound and never lowers it.
// The bound is how long a model may take to answer — the whole generation for a
// non-streaming call — and every call to a known provider, streaming or not,
// goes through its preset client. The presets were written as raises over a
// 30-second default; once the default grew past them, taking them verbatim cut
// openai, azure-openai, azure-foundry and gemini to 30 seconds and groq to 15,
// aborting a reasoning model's non-streaming generation with a transport error.
// A tighter per-attempt bound is the operator's to set, with targets[].timeout.
func applyPreset(base Config, preset ProviderPreset) Config {
	if preset.MaxIdleConnsPerHost > 0 {
		base.MaxIdleConnsPerHost = preset.MaxIdleConnsPerHost
	}
	if preset.ResponseHeaderTimeout > base.ResponseHeaderTimeout {
		base.ResponseHeaderTimeout = preset.ResponseHeaderTimeout
	}
	if preset.DialTimeout > 0 {
		base.DialTimeout = preset.DialTimeout
	}
	return base
}

// RegisterKnownProviders registers isolated pools for all providers in the
// KnownProviderPresets map. Call once at startup after creating the Manager.
func (m *Manager) RegisterKnownProviders() {
	for name, preset := range KnownProviderPresets() {
		cfg := applyPreset(m.cfg, preset)
		m.RegisterProvider(name, cfg)
	}
}
