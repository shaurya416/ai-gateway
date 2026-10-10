package redact

import (
	"strings"
	"testing"
)

// The DSN values below are synthetic. Nothing here reads the process
// environment for a real credential.
const (
	shortPassword        = "brief1"
	syntheticDSNPassword = "s3cr3t-p4ssw0rd-synthetic"
	syntheticPostgresDSN = "postgres://ferrogw:" + syntheticDSNPassword + "@db.internal:5432/gateway"
)

// reseedFromEnv rebuilds the secret set from the current environment, which
// t.Setenv has already adjusted, and restores the previous set afterwards.
func reseedFromEnv(t *testing.T) {
	t.Helper()
	seed()
	prev := secrets.Load()
	secrets.Store(newValueMatcher(envSecretPairs()))
	t.Cleanup(func() { secrets.Store(prev) })
}

// A DSN naming a file is a path, not a credential. The startup line that tells
// an operator which SQLite files to back up must be able to print it.
func TestCredentialIn_FilesystemDSNIsNotASecret(t *testing.T) {
	paths := []string{
		"/data/ferrogw-keys.db",
		"./var/lib/ferrogw/keys.db",
		// _pragma=busy_timeout(...) is the spelling modernc.org/sqlite honours;
		// the _busy_timeout= form other drivers use is silently ignored by it.
		"file:/data/ferrogw-keys.db?_pragma=busy_timeout(5000)",
		"/data/ferrogw-keys.db?_journal_mode=WAL",
		":memory:",
	}
	for _, path := range paths {
		if got := credentialIn("API_KEY_STORE_DSN", path); got != "" {
			t.Errorf("credentialIn(API_KEY_STORE_DSN, %q) = %q, want no secret", path, got)
		}
	}
}

// The password inside a DSN is still a credential, and is what gets enrolled.
func TestCredentialIn_DSNPasswordOnly(t *testing.T) {
	got := credentialIn("CONFIG_STORE_DSN", syntheticPostgresDSN)
	if got != syntheticDSNPassword {
		t.Fatalf("credentialIn = %q, want the userinfo password %q", got, syntheticDSNPassword)
	}

	// A plain credential variable is still enrolled whole.
	if got := credentialIn("MISTRAL_API_KEY", mistralShaped); got != mistralShaped {
		t.Errorf("credentialIn(MISTRAL_API_KEY) = %q, want the whole value", got)
	}
}

// End to end through the value redactor: the backup line stays readable while
// the DSN password does not survive.
func TestValues_KeepsPathRedactsPassword(t *testing.T) {
	t.Setenv("API_KEY_STORE_DSN", "/data/ferrogw-keys.db")
	t.Setenv("CONFIG_STORE_DSN", syntheticPostgresDSN)
	reseedFromEnv(t)

	line := "sqlite admin stores keys=/data/ferrogw-keys.db sessions=/data/ferrogw-keys-sessions.db"
	if got := Values(line); got != line {
		t.Errorf("Values(%q) = %q, want it unchanged", line, got)
	}

	failure := "connect config store: " + syntheticPostgresDSN
	got := Values(failure)
	if strings.Contains(got, syntheticDSNPassword) {
		t.Fatalf("Values leaked the DSN password: %q", got)
	}
	if !strings.Contains(got, "db.internal:5432") {
		t.Errorf("Values(%q) = %q, want the host to stay readable", failure, got)
	}
}

// URLCredentials is the shape-based half, so it also covers a DSN that never
// passed through an environment variable and a password under the value floor.
func TestURLCredentials_UserinfoPassword(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{
			// Under MinSecretLength, so value redaction never enrolled it: the
			// shape rule is what closes this one.
			in:   "dial postgres://ferrogw:" + shortPassword + "@db.internal:5432/gateway failed",
			want: "dial postgres://ferrogw:[REDACTED]@db.internal:5432/gateway failed",
		},
		{
			in:   "redis://default:" + syntheticDSNPassword + "@cache:6379",
			want: "redis://default:[REDACTED]@cache:6379",
		},
		{
			// No userinfo: nothing to redact, everything readable.
			in:   "GET https://api.openai.com/v1/models failed",
			want: "GET https://api.openai.com/v1/models failed",
		},
		{
			// A mailto-shaped string is not a URL with userinfo credentials.
			in:   "contact ops@example.com about http://collector:4318/v1/traces",
			want: "contact ops@example.com about http://collector:4318/v1/traces",
		},
	}
	for _, tc := range cases {
		if got := URLCredentials(tc.in); got != tc.want {
			t.Errorf("URLCredentials(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The default policy set, not just URLCredentials, must remove a userinfo
// password whole. A password may carry characters an email address cannot —
// "!" and "$" are legal in userinfo and common in generated passwords — and a
// dotted host makes the tail of it read as an email address. Matched as one
// first, the email rule consumed the "@" the userinfo rule anchors on, removed
// only the part after the last such character, and left the rest of the
// password in the text.
func TestDefaultRedactor_UserinfoPasswordWithPunctuation(t *testing.T) {
	const (
		// Synthetic, like the values above: "!" and "$" are the characters an
		// email address cannot hold.
		punctuated = "Xk9!mP2$qR7wZ"
		in         = "dial postgres://ferrogw:" + punctuated + "@db.internal.example.com:5432/gateway failed"
		want       = "dial postgres://ferrogw:[REDACTED]@db.internal.example.com:5432/gateway failed"
	)
	for name, redactFn := range map[string]func(string) string{
		"DefaultRedactor": DefaultRedactor().Redact,
		"String":          String,
	} {
		got := redactFn(in)
		for _, fragment := range []string{punctuated, "Xk9!mP2$", "qR7wZ"} {
			if strings.Contains(got, fragment) {
				t.Errorf("%s(%q) = %q, leaks password fragment %q", name, in, got, fragment)
			}
		}
		if got != want {
			t.Errorf("%s(%q)\n got: %q\nwant: %q (scheme, user and host stay readable)", name, in, got, want)
		}
	}
}

// A URL's userinfo ends at the LAST "@" before the host — that is how net/url
// and the drivers built on it read one — so a password or a user name holding a
// raw "@" is a working credential. Ending the userinfo at the first "@" replaced
// the head of such a password and printed its tail, or, for a user name holding
// one, matched nothing and printed the whole password.
func TestRedact_UserinfoHoldingAnAtSign(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   string
		secret []string
	}{
		{
			name:   "password holding an at sign, undotted host",
			in:     "dial postgres://ferrogw:brief@1x@postgres:5432/gateway failed",
			want:   "dial postgres://ferrogw:[REDACTED]@postgres:5432/gateway failed",
			secret: []string{"brief@1x", "1x@"},
		},
		{
			name:   "password holding two at signs, dotted host",
			in:     "proxy https://svc:s3cr@t!x@y9@proxy.internal.example.com:8443/v1 refused",
			want:   "proxy https://svc:[REDACTED]@proxy.internal.example.com:8443/v1 refused",
			secret: []string{"s3cr", "t!x", "y9@"},
		},
		{
			name:   "user name holding an at sign",
			in:     "dial postgres://admin@dbserver:" + shortPassword + "@dbserver.example.com:5432/app failed",
			want:   "dial postgres://admin@dbserver:[REDACTED]@dbserver.example.com:5432/app failed",
			secret: []string{shortPassword},
		},
		{
			// The match ends with the URL: a later field's "@" is not the host's.
			name:   "a JSON line carrying a later address",
			in:     `{"dsn":"postgres://u:` + shortPassword + `@db:5432/app","owner":"ops@example.com"}`,
			want:   `{"dsn":"postgres://u:[REDACTED]@db:5432/app","owner":"ops@example.com"}`,
			secret: []string{shortPassword},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := URLCredentials(tc.in); got != tc.want {
				t.Errorf("URLCredentials(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
			got := DefaultRedactor().Redact(tc.in)
			for _, fragment := range tc.secret {
				if strings.Contains(got, fragment) {
					t.Errorf("DefaultRedactor().Redact(%q) = %q, leaks password fragment %q", tc.in, got, fragment)
				}
			}
		})
	}
}
