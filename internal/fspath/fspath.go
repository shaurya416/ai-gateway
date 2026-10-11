// Package fspath recognises a value that is a filesystem path where a list of
// names was expected.
package fspath

import "strings"

// IsDirectoryPath reports whether v is a filesystem path rather than a model id
// or a comma-separated list of them.
//
// A path has no comma and begins like a path. A model id — "llama3.2",
// "gpt-oss:20b", "library/mistral" — does neither. This is a prefix test only:
// a relative path with no leading "./" ("models", "var/lib/ollama") is
// indistinguishable from a model id and is not caught — but Ollama's own
// default models directory is absolute ($HOME/.ollama/models), which is the
// value that actually reaches a model list by mistake.
func IsDirectoryPath(v string) bool {
	if v == "" || strings.Contains(v, ",") {
		return false
	}
	if strings.HasPrefix(v, "/") || strings.HasPrefix(v, "~") ||
		strings.HasPrefix(v, "./") || strings.HasPrefix(v, "../") ||
		strings.HasPrefix(v, `.\`) || strings.HasPrefix(v, `..\`) {
		return true
	}
	// Windows drive letter: "C:\models", "C:/models". Not "gpt-oss:20b", whose
	// colon is not at index 1 and is not followed by a separator.
	return len(v) >= 3 && v[1] == ':' &&
		(v[2] == '\\' || v[2] == '/') &&
		((v[0] >= 'A' && v[0] <= 'Z') || (v[0] >= 'a' && v[0] <= 'z'))
}
