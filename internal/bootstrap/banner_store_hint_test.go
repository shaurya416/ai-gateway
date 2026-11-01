package bootstrap

import (
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/providers"
)

// The banner's persistence hint has to name the variable that persists the
// store it warned about. It named API_KEY_STORE_BACKEND whatever the warning,
// so a gateway whose key store was already on SQLite but whose config store
// was in memory was told to set the variable it had set, and the variable
// that keeps a PUT /admin/config across a restart, CONFIG_STORE_BACKEND, was
// never mentioned.
func TestStartupBannerNamesTheVariableForEachInMemoryStore(t *testing.T) {
	tests := []struct {
		name        string
		keyStore    string
		configStore string
		want        []string
		wantNot     []string
	}{
		{
			name:        "only the config store in memory",
			keyStore:    BackendSQLite,
			configStore: BackendMemory,
			want:        []string{"CONFIG_STORE_BACKEND"},
			wantNot:     []string{"API_KEY_STORE_BACKEND"},
		},
		{
			name:        "only the key store in memory",
			keyStore:    BackendMemory,
			configStore: BackendPostgres,
			want:        []string{"API_KEY_STORE_BACKEND"},
			wantNot:     []string{"CONFIG_STORE_BACKEND"},
		},
		{
			name:        "both in memory",
			keyStore:    BackendMemory,
			configStore: BackendMemory,
			want:        []string{"API_KEY_STORE_BACKEND", "CONFIG_STORE_BACKEND"},
		},
		{
			name:        "neither in memory",
			keyStore:    BackendSQLite,
			configStore: BackendSQLite,
			wantNot:     []string{"API_KEY_STORE_BACKEND", "CONFIG_STORE_BACKEND"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Config{
				Strategy: config.StrategyConfig{Mode: config.ModeFallback},
				Targets:  []config.Target{{VirtualKey: "openai"}},
			}
			banner := captureStderr(t, func() {
				PrintStartupBanner(":8080", providers.NewRegistry(), &cfg, "k", tt.keyStore, tt.configStore)
			})
			for _, v := range tt.want {
				if !strings.Contains(banner, v) {
					t.Errorf("banner does not name %s:\n%s", v, banner)
				}
			}
			for _, v := range tt.wantNot {
				if strings.Contains(banner, v) {
					t.Errorf("banner names %s, which is not what persists the store it warned about:\n%s", v, banner)
				}
			}
		})
	}
}
