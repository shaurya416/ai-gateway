package run

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// runCLI executes the real command tree with args, recording whether the server
// entrypoint was reached. Both writers go to one buffer because several of these
// assertions are about what lands anywhere at all.
func runCLI(t *testing.T, args ...string) (out *bytes.Buffer, served *bool, err error) {
	t.Helper()
	out = &bytes.Buffer{}
	ran := false
	root := newRootCmd(func() error { ran = true; return nil })
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	err = root.Execute()
	return out, &ran, err
}

// OPS-005. The root is runnable and is also the parent of every subcommand, so
// an unmatched first token must not fall through and boot a gateway — a deploy
// gate spelled slightly wrong would "pass" by starting a server on :8080.
//
// Cobra already refuses it, but only because the root leaves Args nil: Find
// applies legacyArgs to a command whose Args is nil, and legacyArgs rejects an
// unknown token for a root that has subcommands. Give the root ANY Args policy
// and that path is skipped — cobra.NoArgs still rejects but drops the
// "Did you mean this?" line, and cobra.ArbitraryArgs starts the server. The
// suggestion is asserted here because it is the cheap tell that the right
// mechanism is doing the rejecting.
func TestRootRejectsUnknownSubcommand(t *testing.T) {
	out, served, err := runCLI(t, "valdate", "config.yaml")

	if err == nil {
		t.Fatal("an unknown subcommand must exit non-zero")
	}
	if *served {
		t.Fatal("an unknown subcommand started the server instead of failing")
	}
	if !strings.Contains(out.String(), "valdate") {
		t.Errorf("the error must name the token that was not understood:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "validate") {
		t.Errorf("the error must suggest the command that was meant:\n%s", out.String())
	}
}

// The bare `ferrogw` is the documented way to start the server, so the guard
// above must not have cost it.
func TestBareRootStillServes(t *testing.T) {
	_, served, err := runCLI(t)

	if err != nil {
		t.Fatalf("bare ferrogw must start the server, got error: %v", err)
	}
	if !*served {
		t.Fatal("bare ferrogw did not start the server")
	}
}

func TestServeRejectsExtraArguments(t *testing.T) {
	_, served, err := runCLI(t, "serve", "config.yaml")

	if err == nil {
		t.Fatal("`serve` takes no arguments; a stray one must not be swallowed")
	}
	if *served {
		t.Fatal("`serve` ran despite an argument it does not accept")
	}
}

// OPS-011(c). Cobra prints the whole usage block after a failing command — to
// STDOUT, while the error goes to stderr — so the one line saying what went
// wrong was buried under a flag list, and anything reading stdout got the flag
// list instead of nothing.
func TestCommandErrorPrintsNoUsageBlock(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	out, _, err := runCLI(t, "validate", missing)

	if err == nil {
		t.Fatal("validate must fail on a config file that does not exist")
	}
	got := out.String()
	if strings.Contains(got, "Usage:") || strings.Contains(got, "Flags:") {
		t.Errorf("a command error must not drag the usage block with it:\n%s", got)
	}
	if !strings.Contains(got, "load config") {
		t.Errorf("output does not carry the actual error:\n%s", got)
	}
}

func TestServeErrorIsNotPrintedTwice(t *testing.T) {
	out := &bytes.Buffer{}
	root := newRootCmd(func() error { return errors.New("startup failed") })
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"serve"})

	if err := root.Execute(); err == nil {
		t.Fatal("serve must return its startup error")
	}
	if strings.Contains(out.String(), "startup failed") {
		t.Fatalf("Cobra printed an error the startup path already logged: %s", out.String())
	}
}

// `admin keys create` takes its label from --name. Given the label as a
// positional token instead, it used to mint a key with no name — the token went
// to an argument nothing read — print it as created, and exit 0. The token must
// be refused before the Admin API is called.
func TestAdminKeysCreateRefusesAPositionalName(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"new","name":"","key":"fgw_secret"}`))
	}))
	t.Cleanup(srv.Close)

	out, served, err := runCLI(t, "admin", "keys", "create", "ci-bot", "--gateway-url", srv.URL, "--api-key", "k")

	if err == nil {
		t.Fatalf("`admin keys create ci-bot` must fail: the name is read from --name, not from an argument\n%s", out.String())
	}
	if *served {
		t.Fatal("an admin command started the server")
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("the Admin API was called %d times; a key was created with the label dropped", n)
	}
}

// A nil Args is cobra's ArbitraryArgs: the command accepts tokens it will never
// read, which is how a mistyped invocation becomes a silent no-op. This walks
// the root's own children — the commands this binary mounts — so one added
// later is covered without editing this test. It stops at containers rather
// than recursing: `admin`'s leaves are declared in their own file and are that
// file's contract to state.
func TestMountedCommandsDeclareAnArgumentPolicy(t *testing.T) {
	root := newRootCmd(func() error { return nil })
	// The root is the exception, and the opposite way round: see
	// TestRootRejectsUnknownSubcommand.
	if root.Args != nil {
		t.Error("the root must leave Args nil so cobra's legacyArgs rejects an unknown subcommand with a suggestion")
	}
	for _, cmd := range root.Commands() {
		if cmd.Name() == "help" || cmd.Name() == "completion" || cmd.HasSubCommands() {
			continue
		}
		if cmd.Args == nil {
			t.Errorf("command %q declares no Args policy", cmd.Name())
		}
	}
}

// --format names the encoding a script is about to parse. A value no printer
// renders used to fall back to the table silently and exit 0, so `admin keys
// list --format yml` handed a YAML consumer a table it read as one scalar, and
// a typo on a command that mutates — `keys create`, `keys rotate` — was only
// noticed after the change was made. The value must be refused before any
// command runs, which is also before any Admin API call.
func TestUnknownFormatIsRefusedBeforeTheCommandRuns(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"k1","name":"ci","scopes":["read_only"]}]`))
	}))
	t.Cleanup(srv.Close)

	for _, args := range [][]string{
		{"admin", "keys", "list", "--format", "yml"},
		{"admin", "keys", "create", "--name", "ci", "--format", "jsno"},
		{"version", "--format", "xml"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			calls.Store(0)
			out, served, err := runCLI(t, append(args, "--gateway-url", srv.URL, "--api-key", "k")...)

			if err == nil {
				t.Fatalf("an unknown --format must fail the command, got exit 0 with:\n%s", out.String())
			}
			if !strings.Contains(err.Error(), "--format") {
				t.Errorf("the error must name the flag it refused: %v", err)
			}
			if *served {
				t.Fatal("the command started the server")
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("the Admin API was called %d time(s) before the format was checked", n)
			}
		})
	}
}

// The three encodings the printer renders stay accepted, in any letter case,
// as does the default.
func TestKnownFormatsAreAccepted(t *testing.T) {
	for _, format := range []string{"table", "json", "yaml", "JSON", "Yaml"} {
		t.Run(format, func(t *testing.T) {
			out, _, err := runCLI(t, "version", "--format", format)
			if err != nil {
				t.Fatalf("--format %s: %v\n%s", format, err, out.String())
			}
		})
	}
	if out, _, err := runCLI(t, "version"); err != nil {
		t.Fatalf("version with the default format: %v\n%s", err, out.String())
	}
}
