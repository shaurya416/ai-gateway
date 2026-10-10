package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/spf13/cobra"
)

// The gateway picks a config file's decoder from its extension, so a file init
// reports as created has to be one `validate` and `serve` can read. Choosing the
// encoding from --config-format alone wrote YAML into config.json, and a name
// with any other extension, while printing a master key and next steps for it.
func TestRunInitWritesAConfigTheGatewayCanLoad(t *testing.T) {
	loads := []struct {
		name   string
		format string // "" leaves --config-format unset
		file   string
	}{
		{name: "a .json output without --config-format", file: "config.json"},
		{name: "a .yml output without --config-format", file: "config.yml"},
		{name: "json written under a .yaml name", format: "json", file: "config.yaml"},
	}
	for _, tt := range loads {
		t.Run(tt.name, func(t *testing.T) {
			clearProviderEnv(t)
			path := filepath.Join(t.TempDir(), tt.file)
			cmd, errOut := newInitCmd(t)
			setInitFlags(t, cmd, tt.format, path)

			if err := runInit(cmd, nil); err != nil {
				t.Fatalf("runInit: %v\n%s", err, errOut)
			}
			cfg, err := config.LoadConfig(path)
			if err != nil {
				raw, _ := os.ReadFile(path) //nolint:gosec // G304: path is from t.TempDir()
				t.Fatalf("init reported %s created, but the gateway cannot load it: %v\n%s", tt.file, err, raw)
			}
			if err := config.ValidateConfig(*cfg); err != nil {
				t.Fatalf("generated %s does not validate: %v", tt.file, err)
			}
		})
	}

	refused := []struct {
		name   string
		format string
		file   string // "" writes to the default path in the working directory
		want   string
	}{
		{name: "yaml asked for under a .json name", format: "yaml", file: "config.json", want: "read as JSON"},
		{name: "an extension the gateway does not read", file: "gateway.conf", want: ".yaml, .yml or .json"},
		{name: "an unknown --config-format", format: "toml", want: `unsupported --config-format "toml"`},
	}
	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			clearProviderEnv(t)
			dir := t.TempDir()
			t.Chdir(dir)
			path := ""
			if tt.file != "" {
				path = filepath.Join(dir, tt.file)
			}
			cmd, errOut := newInitCmd(t)
			setInitFlags(t, cmd, tt.format, path)

			err := runInit(cmd, nil)
			if err == nil {
				t.Fatalf("runInit succeeded, want a refusal containing %q\n%s", tt.want, errOut)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
			if strings.Contains(errOut.String(), "Master key") {
				t.Errorf("a refused init must not print a master key:\n%s", errOut)
			}
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				t.Fatalf("read dir: %v", readErr)
			}
			if len(entries) != 0 {
				t.Errorf("a refused init must write nothing, found %s", entries[0].Name())
			}
		})
	}
}

// setInitFlags sets init's flags the way a command line would: a flag left
// empty here is a flag the operator did not pass.
func setInitFlags(t *testing.T, cmd *cobra.Command, format, output string) {
	t.Helper()
	flags := map[string]string{"non-interactive": "true"}
	if format != "" {
		flags["config-format"] = format
	}
	if output != "" {
		flags["output"] = output
	}
	for name, value := range flags {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatalf("set --%s: %v", name, err)
		}
	}
}
