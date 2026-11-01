package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// copyRepoTree copies rel (a file or a directory) from the repository root
// into dst, preserving its repository-relative path.
func copyRepoTree(t *testing.T, dst, rel string) {
	t.Helper()
	src := filepath.Join("..", "..", rel)
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		inner, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel, inner)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		data, err := os.ReadFile(path) //nolint:gosec // G304: walking a fixed repository subtree
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600) //nolint:gosec // G703: target sits under the test temp dir
	})
	if err != nil {
		t.Fatalf("copy %s: %v", rel, err)
	}
}

// composeCommand returns the docker compose invocation a Makefile target runs,
// as `make -n` prints it, with its trailing `up ...` replaced by `config`.
func composeCommand(t *testing.T, dir, target string) []string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "make", "-n", "-s", "-C", dir, target).CombinedOutput() //nolint:gosec // G204: fixed make target
	if err != nil {
		t.Fatalf("make -n %s: %v\n%s", target, err, out)
	}
	var line string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "docker compose ") {
			line = strings.TrimSpace(l)
		}
	}
	if line == "" {
		t.Fatalf("make -n %s ran no docker compose command:\n%s", target, out)
	}
	args := strings.Fields(line)
	up := slices.Index(args, "up")
	if up < 0 {
		t.Fatalf("make %s: no `up` in %q", target, line)
	}
	return append(args[:up:up], "config", "--format", "json")
}

// The docs send an operator's settings to one place: "Provider keys go in a
// repository-root .env" (README), "real providers from a repository-root .env"
// (deploy/README.md, compose.fullstack.yaml), "from the repository root that is
// ./.env, not deploy/.env" (compose.prod.yaml). Compose does not read that
// file: it loads .env from the project directory, which is the directory of
// the first -f file — deploy/ since the compose files moved there. So a live
// fullstack started its gateway with every provider key empty, a production
// stack ignored the IMAGE_TAG pin and ran :latest, and no stack's gateway saw
// the MASTER_KEY the file set.
//
// Each Makefile target is rendered with `docker compose config`, exactly as
// make would invoke it, from a copy of the repository holding a root .env.
func TestComposeStacksReadTheRepositoryRootDotEnv(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed")
	}
	if err := exec.CommandContext(t.Context(), "docker", "compose", "version").Run(); err != nil {
		t.Skip("docker compose is not installed")
	}

	root := t.TempDir()
	for _, rel := range []string{"Makefile", "deploy", "config.example.yaml"} {
		copyRepoTree(t, root, rel)
	}
	const (
		masterKey = "fgw_root_dotenv_master_key"
		openAIKey = "sk-root-dotenv"
		imageTag  = "v0.0.0-root-dotenv"
	)
	dotenv := "MASTER_KEY=" + masterKey + "\nOPENAI_API_KEY=" + openAIKey + "\nIMAGE_TAG=" + imageTag + "\n"
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte(dotenv), 0o600); err != nil {
		t.Fatal(err)
	}

	type service struct {
		Image       string            `json:"image"`
		Environment map[string]string `json:"environment"`
	}
	tests := []struct {
		target    string
		service   string
		openAIKey string // "" when the stack substitutes its own
		image     string // "" when the stack builds or does not pin one
	}{
		{target: "up", service: "ferrogw", openAIKey: openAIKey},
		{target: "up-prod", service: "ferrogw", openAIKey: openAIKey, image: "ghcr.io/ferro-labs/ai-gateway:" + imageTag},
		{target: "up-fullstack-live", service: "gateway", openAIKey: openAIKey},
		{target: "up-fullstack", service: "gateway"},
	}
	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			args := composeCommand(t, root, tt.target)
			// The environment of the test process must not supply what the file
			// is supposed to.
			cmd := exec.CommandContext(t.Context(), args[0], args[1:]...) //nolint:gosec // G204: the Makefile's own command
			cmd.Dir = root
			cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
				name, _, _ := strings.Cut(kv, "=")
				return name == "MASTER_KEY" || name == "OPENAI_API_KEY" || name == "IMAGE_TAG" ||
					strings.HasPrefix(name, "COMPOSE_")
			})
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("%s: %v", strings.Join(args, " "), err)
			}
			var project struct {
				Services map[string]service `json:"services"`
			}
			if err := json.Unmarshal(out, &project); err != nil {
				t.Fatalf("decode compose config: %v", err)
			}
			svc, ok := project.Services[tt.service]
			if !ok {
				t.Fatalf("service %q not in the rendered project", tt.service)
			}
			if got := svc.Environment["MASTER_KEY"]; got != masterKey {
				t.Errorf("gateway MASTER_KEY = %q, want %q from the repository-root .env", got, masterKey)
			}
			if tt.openAIKey != "" {
				if got := svc.Environment["OPENAI_API_KEY"]; got != tt.openAIKey {
					t.Errorf("gateway OPENAI_API_KEY = %q, want %q from the repository-root .env", got, tt.openAIKey)
				}
			}
			if tt.image != "" && svc.Image != tt.image {
				t.Errorf("gateway image = %q, want %q (IMAGE_TAG from the repository-root .env)", svc.Image, tt.image)
			}
		})
	}
}
