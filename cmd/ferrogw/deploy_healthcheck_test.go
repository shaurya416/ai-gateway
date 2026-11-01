package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// healthcheckCommand returns the CMD of a Dockerfile's HEALTHCHECK, with its
// line continuations joined.
func healthcheckCommand(t *testing.T, rel string) string {
	t.Helper()
	var instr strings.Builder
	in := false
	for _, line := range strings.Split(repoFile(t, rel), "\n") {
		if !in && !strings.HasPrefix(line, "HEALTHCHECK ") {
			continue
		}
		in = true
		trimmed := strings.TrimSpace(line)
		cont := strings.HasSuffix(trimmed, `\`)
		instr.WriteString(strings.TrimSuffix(trimmed, `\`))
		instr.WriteString(" ")
		if !cont {
			break
		}
	}
	if !in {
		t.Fatalf("%s has no HEALTHCHECK; if it was removed on purpose, update this guard", rel)
	}
	_, cmd, ok := strings.Cut(instr.String(), " CMD ")
	if !ok {
		t.Fatalf("%s: HEALTHCHECK has no CMD: %q", rel, instr.String())
	}
	return strings.TrimSpace(cmd)
}

// The gateway listens on PORT (default 8080), and platforms that run images
// commonly set it. Both images' HEALTHCHECK probed localhost:8080 whatever
// PORT said, so `docker run -e PORT=9000 …` served traffic on 9000 while Docker
// marked the container unhealthy — under Swarm, or a compose service another
// depends on as service_healthy, a ready gateway was replaced or waited on
// forever.
//
// The shell-form CMD is run the way Docker runs it, `/bin/sh -c`, with the
// container's environment and a wget that records the URL it was given.
func TestDockerHealthcheckProbesThePortTheGatewayListensOn(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no POSIX shell")
	}
	bin := t.TempDir()
	stub := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$WGET_ARGS\"\n"
	if err := os.WriteFile(filepath.Join(bin, "wget"), []byte(stub), 0o700); err != nil { //nolint:gosec // G306: an executable test stub
		t.Fatal(err)
	}

	for _, rel := range []string{"deploy/Dockerfile", "deploy/gateway.release.Dockerfile"} {
		cmdline := healthcheckCommand(t, rel)
		for _, tt := range []struct {
			port string // "" leaves PORT unset
			want string
		}{
			{port: "", want: "http://localhost:8080/readyz"},
			{port: "9137", want: "http://localhost:9137/readyz"},
		} {
			t.Run(rel+"/PORT="+tt.port, func(t *testing.T) {
				args := filepath.Join(t.TempDir(), "args")
				env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
					return strings.HasPrefix(kv, "PORT=") || strings.HasPrefix(kv, "PATH=")
				})
				env = append(env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "WGET_ARGS="+args)
				if tt.port != "" {
					env = append(env, "PORT="+tt.port)
				}
				cmd := exec.Command(sh, "-c", cmdline) //nolint:gosec // G204: the Dockerfile's own HEALTHCHECK
				cmd.Env = env
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("HEALTHCHECK %q: %v\n%s", cmdline, err, out)
				}
				got, err := os.ReadFile(args) //nolint:gosec // G304: test temp file
				if err != nil {
					t.Fatalf("HEALTHCHECK %q never ran wget: %v", cmdline, err)
				}
				if !slices.Contains(strings.Fields(string(got)), tt.want) {
					t.Errorf("HEALTHCHECK %q probed %q, want %s", cmdline, strings.Fields(string(got)), tt.want)
				}
			})
		}
	}
}
