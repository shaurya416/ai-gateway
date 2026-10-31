package redisstore

import (
	"strings"
	"testing"
)

func TestParseURL_ReadsEveryPart(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		raw  string
		want Endpoint
	}{
		{"redis://cache", Endpoint{Addr: "cache:6379", Host: "cache"}},
		{"redis://cache/", Endpoint{Addr: "cache:6379", Host: "cache"}},
		{"redis://cache:6380/2", Endpoint{Addr: "cache:6380", Host: "cache", DB: 2}},
		{"rediss://cache.example.com:6380", Endpoint{Addr: "cache.example.com:6380", Host: "cache.example.com", TLS: true}},
		{"REDISS://cache", Endpoint{Addr: "cache:6379", Host: "cache", TLS: true}},
		{"redis://:s3cret@cache", Endpoint{Addr: "cache:6379", Host: "cache", Password: "s3cret"}},
		{"redis://app:s3cret@cache/1", Endpoint{Addr: "cache:6379", Host: "cache", Username: "app", Password: "s3cret", DB: 1}},
		// A reserved character in the password is percent-encoded, and the
		// decoded password is what AUTH sends.
		{"redis://:p%40ss%2Fw%3Ard%25@cache", Endpoint{Addr: "cache:6379", Host: "cache", Password: "p@ss/w:rd%"}}, //nolint:gosec // G101: the decoding of a reserved character is under test, not a credential
		{"redis://[::1]:7000/15", Endpoint{Addr: "[::1]:7000", Host: "::1", DB: 15}},
		// An empty userinfo is no credential at all.
		{"redis://:@cache", Endpoint{Addr: "cache:6379", Host: "cache"}},
	} {
		got, err := ParseURL(tc.raw)
		if err != nil {
			t.Errorf("ParseURL(%q) = %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseURL(%q) = %+v, want %+v", tc.raw, got, tc.want)
		}
	}
}

// Each of these is refused rather than read some other way. The password
// "s3cret" is in most of them, and no error may quote it: these errors are
// printed by `ferrogw validate` and logged at startup.
func TestParseURL_RefusesWithoutQuotingTheURL(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, raw, want string
	}{
		{"no scheme", "cache:6379", "redis:// or rediss://"},
		{"no scheme, credential first", "s3cret:x@cache", "redis:// or rediss://"},
		{"wrong scheme", "http://:s3cret@cache", "redis:// or rediss://"},
		{"unix socket", "unix:///var/run/redis.sock", "redis:// or rediss://"},
		{"opaque", "redis:s3cret@cache", "two slashes"},
		{"query string", "redis://:s3cret@cache?ssl_cert_reqs=none", "query string"},
		{"empty query", "redis://:s3cret@cache?", "query string"},
		{"fragment", "redis://:s3cret@cache#main", "fragment"},
		{"no host", "redis://:s3cret@:6379", "no host"},
		{"port zero", "redis://:s3cret@cache:0", "port"},
		{"port out of range", "redis://:s3cret@cache:70000", "port"},
		{"database not a number", "redis://:s3cret@cache/main", "database number"},
		{"negative database", "redis://:s3cret@cache/-1", "database number"},
		{"nested path", "redis://:s3cret@cache/0/1", "database number"},
		{"username without password", "redis://s3cret@cache", "no password"},
		// An unescaped / or # in the password cuts the authority short, and
		// the parser's own error then quotes the start of the password.
		{"unescaped slash in password", "redis://:s3cret/x@cache", "percent-encode"},
		{"unescaped hash in password", "redis://:s3cret#x@cache", "percent-encode"},
		{"bad escape in password", "redis://:s3cret%zz@cache", "percent-encode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseURL(tc.raw)
			if err == nil {
				t.Fatalf("ParseURL(%q) accepted it", tc.raw)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "s3cret") {
				t.Errorf("error %q quotes the credential", err)
			}
		})
	}
}

func TestEndpointString_LeavesTheCredentialsOut(t *testing.T) {
	t.Parallel()

	ep, err := ParseURL("rediss://app:s3cret@cache:6380/4")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ep.String(), "rediss://cache:6380/4"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
