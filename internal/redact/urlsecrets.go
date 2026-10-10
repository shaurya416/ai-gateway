package redact

import (
	"regexp"
	"strings"
)

// urlCredentialParam matches the value of a credential-named query parameter.
//
// Keyword-anchored for the same reason keyword_secret is: a query value has no
// distinguishing shape, so the parameter *name* is the whole signal. That keeps
// "?limit=10" and "?model=gpt-4o" readable in logs while "?access_token=…"
// does not survive. The value class stops at the next separator so the rest of
// the URL — and any surrounding JSON quoting — is preserved.
var urlCredentialParam = regexp.MustCompile(
	`(?i)([?&](?:access[_\-]?token|api[_\-]?key|apikey|auth|authorization|token|key|secret|password|signature|sig|credential)=)[^&\s"'\\]+`,
)

// urlUserinfoPassword matches the password half of a URL's userinfo section —
// the shape a database DSN carries: postgres://user:pw@host/db.
//
// Only the password is replaced. The scheme, user and host stay readable,
// because "connection refused" is unanswerable without them, and neither is a
// secret. The match stops at the "@" before the host so the rest of the URL
// survives.
//
// The userinfo ends at the LAST "@" before the host, not the first: that is how
// net/url and the database drivers built on it read a URL, so a password or a
// user name carrying a raw "@" — common in generated passwords, and the shape
// some managed databases give their user names — is a working credential. Read
// up to the first "@" instead, the rule replaced a password's head and printed
// the rest of it, or matched nothing and printed all of it. A segment after the
// first "@" stops at a quotation mark, which no URL's userinfo can hold, so the
// match cannot run on into the next field of a JSON line.
var urlUserinfoPassword = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^\s:/?#@]+(?:@[^\s:/?#@"]+)*:)[^\s/?#@]+(?:@[^\s/?#@"]+)*@`)

// URLCredentials replaces credential-bearing query-parameter values in s, and
// the password in a URL's userinfo.
//
// This is the one credential shape that value redaction cannot reach on its
// own: a token written literally into the config file rather than as a ${VAR}
// reference was never an environment value, so the gateway holds it only as
// config text. It still ends up in a log line the moment that URL is dialled
// and the dial fails.
//
// Cheap enough for every log line: a substring scan rejects anything that is
// not a URL before either regex is run against the input, and the parameter
// rule additionally requires a "=".
func URLCredentials(s string) string {
	if !strings.Contains(s, "://") {
		return s
	}
	if strings.Contains(s, "=") {
		s = urlCredentialParam.ReplaceAllString(s, "${1}"+redactedURLValue)
	}
	if strings.Contains(s, "@") {
		s = urlUserinfoPassword.ReplaceAllString(s, "${1}"+redactedURLValue+"@")
	}
	return s
}

const redactedURLValue = "[REDACTED]"
