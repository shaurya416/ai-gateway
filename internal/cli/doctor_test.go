package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunDoctor(t *testing.T) {
	// clearProviderKeys blanks every provider env var doctor probes so host
	// environment leakage does not skew the "N found" count.
	clearProviderKeys := func(t *testing.T) {
		for _, k := range []string{
			"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY",
			"GROQ_API_KEY", "MISTRAL_API_KEY",
		} {
			t.Setenv(k, "")
		}
	}

	t.Run("reports keys, config, auth and healthy connectivity", func(t *testing.T) {
		srv := stubGateway(t, map[string]http.HandlerFunc{
			"/health": jsonHandler(http.StatusOK, `{"status":"ok"}`),
		})
		cmd, out := newHandlerCmd(t, srv.URL, "table")
		clearProviderKeys(t)
		t.Setenv("OPENAI_API_KEY", "sk-test")
		t.Setenv("GATEWAY_CONFIG", "")
		t.Setenv("MASTER_KEY", "master-test")

		if err := runDoctor(cmd, nil); err != nil {
			t.Fatalf("runDoctor: %v", err)
		}

		got := out.String()
		for _, want := range []string{
			"Provider API Keys", "openai", "1 found",
			"MASTER_KEY is set", "healthy",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("output missing %q:\n%s", want, got)
			}
		}
	})

	t.Run("flags no keys and an invalid config file", func(t *testing.T) {
		cfgPath := filepath.Join(t.TempDir(), "bad.yaml")
		// Valid YAML, invalid config: an unknown strategy mode fails validation.
		if err := os.WriteFile(cfgPath, []byte("strategy:\n  mode: bogus\ntargets:\n  - virtual_key: openai\n"), 0600); err != nil {
			t.Fatalf("write config: %v", err)
		}

		cmd, out := newHandlerCmd(t, "http://127.0.0.1:1", "table")
		clearProviderKeys(t)
		t.Setenv("GATEWAY_CONFIG", cfgPath)
		t.Setenv("MASTER_KEY", "")

		// An invalid config is the one finding doctor exits non-zero on, so a
		// pipeline gating on it fails rather than passing everything.
		if err := runDoctor(cmd, nil); err == nil {
			t.Fatal("runDoctor returned nil for a config validate rejects")
		}

		got := out.String()
		for _, want := range []string{
			"no provider API keys detected",
			cfgPath,
			"MASTER_KEY not set",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("output missing %q:\n%s", want, got)
			}
		}
	})

	// doctor and validate must agree about a config. doctor used to run only
	// LoadConfig + ValidateConfig, so a target naming a provider that does not
	// exist — the thing validate exists to catch — was reported green by the
	// command an operator reaches for when something is already wrong.
	t.Run("reports a config validate would reject", func(t *testing.T) {
		cases := map[string]string{
			"bad.yaml": "strategy:\n  mode: single\ntargets:\n  - virtual_key: not-a-real-provider\n",
			"multistage.yaml": "strategy:\n  mode: single\ntargets:\n  - virtual_key: openai\n" +
				"plugins:\n  - name: response-cache\n    type: cache\n" +
				"    stage: before_request\n    enabled: true\n    config:\n      max_age: 300\n" +
				"  - name: response-cache\n    type: cache\n" +
				"    stage: after_request\n    enabled: true\n    config:\n      max_age: 301\n",
		}
		for name, body := range cases {
			t.Run(name, func(t *testing.T) {
				cfgPath := filepath.Join(t.TempDir(), name)
				if err := os.WriteFile(cfgPath, []byte(body), 0600); err != nil {
					t.Fatalf("write config: %v", err)
				}

				cmd, out := newHandlerCmd(t, "http://127.0.0.1:1", "table")
				clearProviderKeys(t)
				t.Setenv("GATEWAY_CONFIG", cfgPath)

				if err := runDoctor(cmd, nil); err == nil {
					t.Fatal("runDoctor returned nil for a config validate rejects")
				}
				// The FAIL must be on the CONFIG line: the unreachable gateway in
				// this test prints one of its own, so asserting on the whole
				// output would pass without doctor checking the config at all.
				got := out.String()
				failedOnConfig := false
				for _, line := range strings.Split(got, "\n") {
					if strings.Contains(line, cfgPath) && strings.Contains(line, SymFAIL) {
						failedOnConfig = true
					}
				}
				if !failedOnConfig {
					t.Errorf("doctor reported no config failure for a config validate rejects:\n%s", got)
				}
			})
		}
	})

	// OPS-011(a): --format is inherited from the root and doctor cannot honour
	// it — its output is a diagnosis to read, not a record. It used to be
	// ignored, so `ferrogw doctor --format json` emitted the same human report
	// and any parser downstream failed on it.
	t.Run("a non-table --format is refused rather than ignored", func(t *testing.T) {
		cmd, out := newHandlerCmd(t, "http://127.0.0.1:1", "yaml")
		clearProviderKeys(t)
		t.Setenv("GATEWAY_CONFIG", "")

		err := runDoctor(cmd, nil)
		if err == nil {
			t.Fatal("runDoctor must refuse --format yaml rather than emit human text")
		}
		if !strings.Contains(err.Error(), "--format yaml") {
			t.Errorf("error = %v, want it to name the rejected format", err)
		}
		if out.String() != "" {
			t.Errorf("want no output when the format is refused, got:\n%s", out.String())
		}
	})
}

// doctor answers "will this deployment work", so it has to read the
// environment the way `serve` reads it. serve trims GATEWAY_CONFIG and
// MASTER_KEY; doctor read both verbatim. A GATEWAY_CONFIG carrying a trailing
// space — which an env file keeps — made doctor exit 1 with "configuration is
// invalid" for a file serve loads, and a whitespace-only MASTER_KEY was
// reported set while serve runs with no master key at all.
func TestRunDoctorReadsTheEnvironmentAsServeDoes(t *testing.T) {
	srv := stubGateway(t, map[string]http.HandlerFunc{
		"/health": jsonHandler(http.StatusOK, `{"status":"ok"}`),
	})

	t.Run("a GATEWAY_CONFIG with surrounding whitespace names the file", func(t *testing.T) {
		cfgPath := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(cfgPath, []byte("strategy:\n  mode: single\ntargets:\n  - virtual_key: openai\n"), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		cmd, out := newHandlerCmd(t, srv.URL, "table")
		t.Setenv("GATEWAY_CONFIG", cfgPath+" \n")

		if err := runDoctor(cmd, nil); err != nil {
			t.Fatalf("doctor failed a config serve loads: %v\n%s", err, out.String())
		}
		if !strings.Contains(out.String(), "strategy=single") {
			t.Errorf("doctor did not report the config file it was pointed at:\n%s", out.String())
		}
	})

	t.Run("a whitespace-only GATEWAY_CONFIG is unset", func(t *testing.T) {
		cmd, out := newHandlerCmd(t, srv.URL, "table")
		t.Setenv("GATEWAY_CONFIG", "  ")

		if err := runDoctor(cmd, nil); err != nil {
			t.Fatalf("serve runs on defaults here; doctor failed: %v\n%s", err, out.String())
		}
		if !strings.Contains(out.String(), "GATEWAY_CONFIG not set") {
			t.Errorf("doctor did not report the defaults serve uses:\n%s", out.String())
		}
	})

	t.Run("a whitespace-only MASTER_KEY is not set", func(t *testing.T) {
		cmd, out := newHandlerCmd(t, srv.URL, "table")
		t.Setenv("GATEWAY_CONFIG", "")
		t.Setenv("MASTER_KEY", " \t")

		if err := runDoctor(cmd, nil); err != nil {
			t.Fatalf("runDoctor: %v", err)
		}
		if strings.Contains(out.String(), "MASTER_KEY is set") || !strings.Contains(out.String(), "MASTER_KEY not set") {
			t.Errorf("doctor reported a master key serve does not have:\n%s", out.String())
		}
	})
}
