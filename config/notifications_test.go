package config_test

import (
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
)

func notificationsConfig(hooks ...config.WebhookConfig) config.Config {
	return config.Config{
		Targets:       []config.Target{{VirtualKey: "openai"}},
		Notifications: &config.NotificationsConfig{Webhooks: hooks},
	}
}

func validHook() config.WebhookConfig {
	return config.WebhookConfig{
		Name:   "oncall",
		URL:    "https://alerts.example.com/hooks/ferro",
		Events: []string{config.WebhookEventCircuitOpened, config.WebhookEventBudgetExceeded},
	}
}

func TestValidateNotifications_Accepts(t *testing.T) {
	withRate := validHook()
	withRate.Name = "rates"
	withRate.Events = []string{config.WebhookEventErrorRateHigh}
	withRate.ErrorRate = &config.ErrorRateConfig{Threshold: 0.25, Window: "30s", MinRequests: 5}

	referenced := validHook()
	referenced.Name = "referenced"
	referenced.URL = "${ONCALL_WEBHOOK_URL}"
	referenced.Secret = "${ONCALL_WEBHOOK_SECRET}"
	referenced.Timeout = "2s"

	plainHTTP := validHook()
	plainHTTP.Name = "plain"
	plainHTTP.URL = "http://localhost:9000/alerts"

	cases := map[string]config.Config{
		"no block":         {Targets: []config.Target{{VirtualKey: "openai"}}},
		"empty block":      notificationsConfig(),
		"literal url":      notificationsConfig(validHook()),
		"error rate":       notificationsConfig(withRate),
		"references":       notificationsConfig(referenced),
		"http":             notificationsConfig(plainHTTP),
		"every event":      notificationsConfig(config.WebhookConfig{Name: "all", URL: "https://h/x", Events: config.WebhookEvents(), ErrorRate: &config.ErrorRateConfig{Threshold: 1}}),
		"several webhooks": notificationsConfig(validHook(), withRate, referenced),
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if err := config.ValidateConfig(cfg); err != nil {
				t.Fatalf("ValidateConfig: %v", err)
			}
		})
	}
}

func TestValidateNotifications_Rejects(t *testing.T) {
	mutate := func(fn func(*config.WebhookConfig)) config.WebhookConfig {
		w := validHook()
		fn(&w)
		return w
	}
	rate := func(e config.ErrorRateConfig) config.WebhookConfig {
		return mutate(func(w *config.WebhookConfig) {
			w.Events = []string{config.WebhookEventErrorRateHigh}
			w.ErrorRate = &e
		})
	}

	cases := []struct {
		name  string
		hooks []config.WebhookConfig
		want  string
	}{
		{"empty name", []config.WebhookConfig{mutate(func(w *config.WebhookConfig) { w.Name = "" })}, "name must not be empty"},
		{"padded name", []config.WebhookConfig{mutate(func(w *config.WebhookConfig) { w.Name = " oncall" })}, "surrounding whitespace"},
		{"duplicate name", []config.WebhookConfig{validHook(), validHook()}, "listed more than once"},
		{"empty url", []config.WebhookConfig{mutate(func(w *config.WebhookConfig) { w.URL = "" })}, "url must not be empty"},
		{"ftp url", []config.WebhookConfig{mutate(func(w *config.WebhookConfig) { w.URL = "ftp://h/x" })}, "http or https"},
		{"relative url", []config.WebhookConfig{mutate(func(w *config.WebhookConfig) { w.URL = "/hooks" })}, "http or https"},
		{"no host", []config.WebhookConfig{mutate(func(w *config.WebhookConfig) { w.URL = "https:///hooks" })}, "must name a host"},
		{"fragment", []config.WebhookConfig{mutate(func(w *config.WebhookConfig) { w.URL = "https://h/x#frag" })}, "fragment"},
		{"no events", []config.WebhookConfig{mutate(func(w *config.WebhookConfig) { w.Events = nil })}, "at least one"},
		{"unknown event", []config.WebhookConfig{mutate(func(w *config.WebhookConfig) { w.Events = []string{"circuit.open"} })}, `unknown event "circuit.open"`},
		{"duplicate event", []config.WebhookConfig{mutate(func(w *config.WebhookConfig) {
			w.Events = []string{config.WebhookEventCircuitOpened, config.WebhookEventCircuitOpened}
		})}, "listed more than once"},
		{"literal secret", []config.WebhookConfig{mutate(func(w *config.WebhookConfig) { w.Secret = "hunter2" })}, "must be a ${VAR} reference"},
		{"secret with literal around a reference", []config.WebhookConfig{mutate(func(w *config.WebhookConfig) { w.Secret = "x${S}" })}, "must be a ${VAR} reference"},
		{"bad timeout", []config.WebhookConfig{mutate(func(w *config.WebhookConfig) { w.Timeout = "5" })}, "not a duration"},
		{"zero timeout", []config.WebhookConfig{mutate(func(w *config.WebhookConfig) { w.Timeout = "0s" })}, "must be positive"},
		{"error rate without block", []config.WebhookConfig{mutate(func(w *config.WebhookConfig) {
			w.Events = []string{config.WebhookEventErrorRateHigh}
		})}, "needs an error_rate block"},
		{"block without error rate", []config.WebhookConfig{mutate(func(w *config.WebhookConfig) {
			w.ErrorRate = &config.ErrorRateConfig{Threshold: 0.5}
		})}, "would never fire"},
		{"zero threshold", []config.WebhookConfig{rate(config.ErrorRateConfig{})}, "threshold must be a fraction"},
		{"threshold above one", []config.WebhookConfig{rate(config.ErrorRateConfig{Threshold: 50})}, "threshold must be a fraction"},
		{"bad window", []config.WebhookConfig{rate(config.ErrorRateConfig{Threshold: 0.5, Window: "soon"})}, "not a duration"},
		{"negative window", []config.WebhookConfig{rate(config.ErrorRateConfig{Threshold: 0.5, Window: "-1m"})}, "must be positive"},
		{"negative min requests", []config.WebhookConfig{rate(config.ErrorRateConfig{Threshold: 0.5, MinRequests: -1})}, "cannot be negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := config.ValidateConfig(notificationsConfig(tc.hooks...))
			if err == nil {
				t.Fatalf("want an error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestValidateWebhookURL_DoesNotQuoteTheURL: a URL check on a value
// that was never in the config — one resolved from the environment — must not
// echo it, because it can carry a token.
func TestValidateWebhookURL_DoesNotQuoteTheURL(t *testing.T) {
	err := config.ValidateWebhookURL("https://h/hooks/SECRET-TOKEN\x7f")
	if err == nil {
		t.Fatal("a URL with a control character was accepted")
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Fatalf("error quotes the URL: %v", err)
	}
}

func TestLoadConfig_NotificationsBothFormats(t *testing.T) {
	yamlDoc := `
targets:
  - virtual_key: openai
notifications:
  webhooks:
    - name: oncall
      url: ${ONCALL_WEBHOOK_URL}
      secret: ${ONCALL_WEBHOOK_SECRET}
      events: [circuit.opened, circuit.closed, error_rate.high]
      timeout: 3s
      error_rate:
        threshold: 0.5
        window: 1m
        min_requests: 20
`
	jsonDoc := `{"targets":[{"virtual_key":"openai"}],"notifications":{"webhooks":[{"name":"oncall","url":"${ONCALL_WEBHOOK_URL}","secret":"${ONCALL_WEBHOOK_SECRET}","events":["circuit.opened","circuit.closed","error_rate.high"],"timeout":"3s","error_rate":{"threshold":0.5,"window":"1m","min_requests":20}}]}}`

	for name, doc := range map[string]string{"c.yaml": yamlDoc, "c.json": jsonDoc} {
		path := writeTempFile(t, name, doc)
		cfg, err := config.LoadConfig(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := config.ValidateConfig(*cfg); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		w := cfg.Notifications.Webhooks[0]
		// References are kept as written: resolution happens at construction.
		if w.URL != "${ONCALL_WEBHOOK_URL}" || w.Secret != "${ONCALL_WEBHOOK_SECRET}" {
			t.Fatalf("%s: references were resolved at load: %+v", name, w)
		}
		if w.ErrorRate == nil || w.ErrorRate.MinimumRequests() != 20 || w.ErrorRate.WindowDuration().String() != "1m0s" {
			t.Fatalf("%s: error_rate = %+v", name, w.ErrorRate)
		}
		if w.WebhookTimeout().String() != "3s" {
			t.Fatalf("%s: timeout = %v", name, w.WebhookTimeout())
		}
	}

	// A misspelled key is refused by the strict decoder.
	path := writeTempFile(t, "typo.yaml", strings.Replace(yamlDoc, "min_requests", "min_request", 1))
	if _, err := config.LoadConfig(path); err == nil {
		t.Fatal("a misspelled notifications key was accepted")
	}
}

func TestWebhookDefaults(t *testing.T) {
	var w config.WebhookConfig
	if w.WebhookTimeout() != config.DefaultWebhookTimeout {
		t.Errorf("timeout default = %v", w.WebhookTimeout())
	}
	var e config.ErrorRateConfig
	if e.WindowDuration() != config.DefaultErrorRateWindow || e.MinimumRequests() != config.DefaultErrorRateMinRequests {
		t.Errorf("error rate defaults = %v, %d", e.WindowDuration(), e.MinimumRequests())
	}
}
