package bootstrap

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// envExampleAssignment matches a NAME=value line of .env.example, live or
// commented out with a leading "# " for the operator to uncomment.
var envExampleAssignment = regexp.MustCompile(`^(?:#\s*)?([A-Z][A-Z0-9_]*)=(.*)$`)

// envExampleLines returns every assignment in .env.example as the operator
// gets it by uncommenting the line: name and value, the value read the way
// `docker run --env-file` and `kubectl create secret --from-env-file` read it —
// everything after the first "=", untrimmed, with no comment syntax. Those are
// two of the loaders the file's header names, and neither has inline comments.
func envExampleLines(t *testing.T) [][2]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", ".env.example"))
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}
	var out [][2]string
	for _, line := range strings.Split(string(data), "\n") {
		if m := envExampleAssignment.FindStringSubmatch(line); m != nil {
			out = append(out, [2]string{m[1], m[2]})
		}
	}
	if len(out) == 0 {
		t.Fatal(".env.example has no NAME=value lines; if its format changed, update this guard")
	}
	return out
}

// A trailing "# note" on a NAME=value line is a comment to a shell's `source`
// and to Compose, and part of the value to the two literal readers. So
// uncommenting `GATEWAY_ENV=production        # turns on production-mode
// startup safety checks` and starting the container with
// `docker run --env-file .env` set GATEWAY_ENV to the whole line, which is not
// "production": every production refusal and warning was off in a deployment
// that had asked for them. The same shape corrupted a Databricks token, an
// Azure api-version, the Replicate and Ollama model lists, and the OTLP
// endpoint.
func TestEnvExampleValuesCarryNoInlineComment(t *testing.T) {
	for _, kv := range envExampleLines(t) {
		name, value := kv[0], kv[1]
		if strings.Contains(value, "#") {
			t.Errorf(".env.example: %s=%q — docker run --env-file and kubectl --from-env-file read "+
				"the comment as part of the value; put it on its own line", name, value)
		}
	}
}

func TestEnvExampleProductionLineEnablesProductionMode(t *testing.T) {
	var found bool
	for _, kv := range envExampleLines(t) {
		if kv[0] != "GATEWAY_ENV" {
			continue
		}
		found = true
		t.Setenv("GATEWAY_ENV", kv[1])
		if !IsProduction() {
			t.Errorf("GATEWAY_ENV=%q, as docker run --env-file reads the .env.example line, "+
				"does not turn production mode on", kv[1])
		}
	}
	if !found {
		t.Fatal(".env.example no longer documents GATEWAY_ENV")
	}
}

// The header's `docker run` line has to name an image the release publishes,
// and publish the port, or following it fails before the file is read at all.
func TestEnvExampleDockerRunUsesThePublishedImage(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".env.example"))
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}
	var run string
	for _, line := range strings.Split(string(data), "\n") {
		if _, after, ok := strings.Cut(line, "docker run "); ok {
			run = after
			break
		}
	}
	if run == "" {
		t.Fatal(".env.example no longer shows a docker run command; if it moved, update this guard")
	}

	release, err := os.ReadFile(filepath.Join("..", "..", ".goreleaser.yaml"))
	if err != nil {
		t.Fatalf("read .goreleaser.yaml: %v", err)
	}
	var gr struct {
		Dockers []struct {
			Images []string `yaml:"images"`
		} `yaml:"dockers_v2"`
	}
	if err := yaml.Unmarshal(release, &gr); err != nil {
		t.Fatalf("parse .goreleaser.yaml: %v", err)
	}
	var published []string
	for _, d := range gr.Dockers {
		published = append(published, d.Images...)
	}
	if len(published) == 0 {
		t.Fatal(".goreleaser.yaml publishes no image under dockers_v2; update this guard")
	}

	fields := strings.Fields(run)
	image := fields[len(fields)-1]
	if name, _, _ := strings.Cut(image, ":"); !slices.Contains(published, name) {
		t.Errorf(".env.example runs image %q; the release publishes %v", image, published)
	}
	if !strings.Contains(run, "-p 8080:8080") {
		t.Errorf(".env.example docker run %q publishes no port, so the gateway is unreachable", run)
	}
}
