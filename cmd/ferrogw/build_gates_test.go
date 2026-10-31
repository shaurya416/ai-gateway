package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// repoFile reads a path relative to the repository root.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", rel)) //nolint:gosec // G304: fixed repository-relative path
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// unitTestTimeout finds the -timeout on the unit-test invocation (race + short)
// in the given file and returns it in seconds.
func unitTestTimeout(t *testing.T, rel string) int {
	t.Helper()
	re := regexp.MustCompile(`go test -v -short -race -timeout (\d+)s`)
	m := re.FindStringSubmatch(repoFile(t, rel))
	if m == nil {
		t.Fatalf("%s no longer contains a `go test -v -short -race -timeout <n>s` invocation; "+
			"if it was renamed, update this guard rather than deleting it", rel)
	}
	secs, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("%s: unparsable timeout %q: %v", rel, m[1], err)
	}
	return secs
}

// OPS-013. `make test` and CI run the same suite, so the local gate must not be
// killed by a bound CI already found too tight: the Makefile sat at 180s long
// after ci.yml was raised to 300s for exactly that reason, and the resulting
// timeout — which names no failing test — was investigated as a regression by
// three separate people.
//
// Local may exceed CI (a developer machine is the loaded one); it may not sit
// under it.
func TestLocalTestTimeoutIsNotTighterThanCI(t *testing.T) {
	local := unitTestTimeout(t, "Makefile")
	ci := unitTestTimeout(t, ".github/workflows/ci.yml")

	if local < ci {
		t.Errorf("make test uses -timeout %ds but CI uses -timeout %ds; "+
			"the local gate would fail runs CI passes", local, ci)
	}
}

// GATEWAY_PACKAGES is spelled out rather than ./..., because web/node_modules
// can carry third-party Go sources, so a package directory added at the top of
// the module is outside `make test` until someone lists it. client/, run/ and
// httpgateway/ all were, and the local gate reported success without running
// them while CI, which runs ./..., did.
func TestMakeTestListsEveryPackage(t *testing.T) {
	re := regexp.MustCompile(`(?m)^GATEWAY_PACKAGES = \$\(shell go list ([^)]*)\)$`)
	m := re.FindStringSubmatch(repoFile(t, "Makefile"))
	if m == nil {
		t.Fatal("Makefile no longer sets GATEWAY_PACKAGES from `go list <patterns>`; " +
			"if it was renamed, update this guard rather than deleting it")
	}
	listed := map[string]bool{}
	for _, pattern := range strings.Fields(m[1]) {
		listed[pattern] = true
	}

	root := filepath.Join("..", "..")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read repository root: %v", err)
	}
	if hasModuleGoFile(t, root, false) && !listed["."] {
		t.Error(`GATEWAY_PACKAGES does not list ".", the root package`)
	}
	for _, e := range entries {
		// web/ is the one directory left out on purpose: it is the reason the
		// list is spelled out.
		if !e.IsDir() || goToolIgnores(e.Name()) || e.Name() == "web" || e.Name() == "vendor" {
			continue
		}
		if pattern := "./" + e.Name() + "/..."; hasModuleGoFile(t, filepath.Join(root, e.Name()), true) && !listed[pattern] {
			t.Errorf("GATEWAY_PACKAGES does not list %s, so make test never runs its tests", pattern)
		}
	}
}

// goToolIgnores reports whether the go tool skips a directory of this name
// when it expands a ./... pattern.
func goToolIgnores(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata"
}

// hasModuleGoFile reports whether dir holds a .go file of this module: one
// the go tool does not ignore and that no nested go.mod claims for another
// module. With recurse false only dir itself is read.
func hasModuleGoFile(t *testing.T, dir string, recurse bool) bool {
	t.Helper()
	found := false
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == dir {
				return nil
			}
			if !recurse || goToolIgnores(d.Name()) {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".go") {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return found
}
