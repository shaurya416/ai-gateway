package core

import (
	"net/url"
	"strings"
	"testing"
	"unicode"
)

// FuzzResolveAPIRoot holds the base-URL rule on any configured value: a root
// it accepts keeps the scheme, credentials and host the operator wrote, carries
// no query or fragment an operation path could be buried in, ends in no slash,
// and resolves to itself, so the stored root cannot be reinterpreted.
func FuzzResolveAPIRoot(f *testing.F) {
	for _, seed := range []string{
		"", "https://api.openai.com/v1", "http://host:9901", "http://host:9901/", "https://user:p%40ss@proxy.example.com",
		"https://host/openai", "https://host/v1?a=b", "https://host#", "https://[::1]:8080", " https://host/v1/ ",
	} {
		f.Add(seed, "https://api.groq.com/openai/v1")
		f.Add(seed, "https://generativelanguage.googleapis.com/v1beta")
	}
	f.Fuzz(func(t *testing.T, configured, defaultRoot string) {
		root, err := ResolveAPIRoot("fuzz", configured, defaultRoot)
		// Whitespace inside a URL is not a URL (RFC 3986 has no spelling for
		// it), so only the surrounding whitespace a config line picks up is
		// held to the rule.
		if err != nil || strings.TrimSpace(configured) == "" || strings.ContainsFunc(strings.TrimSpace(configured), unicode.IsSpace) {
			return
		}
		in, _ := url.Parse(strings.TrimSpace(configured))
		out, err := url.Parse(root)
		if err != nil {
			t.Fatalf("resolved root %q does not parse: %v", root, err)
		}
		if out.Scheme != in.Scheme || out.Host != in.Host || out.User.String() != in.User.String() {
			t.Fatalf("%q resolved to %q, which changes the scheme, host or credentials", configured, root)
		}
		if out.RawQuery != "" || out.Fragment != "" || strings.HasSuffix(root, "/") {
			t.Fatalf("%q resolved to %q", configured, root)
		}
		again, err := ResolveAPIRoot("fuzz", root, defaultRoot)
		if err != nil || again != root {
			t.Fatalf("%q resolved to %q, which resolves to %q (%v)", configured, root, again, err)
		}
	})
}
