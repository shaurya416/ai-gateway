package core

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// FuzzParseRetryAfter holds ParseRetryAfter to its godoc on any header value:
// never a negative wait, and a positive delta-seconds value is that many
// seconds exactly.
func FuzzParseRetryAfter(f *testing.F) {
	for _, seed := range []string{"", "0", "1", "120", " 7 ", "-5", "9223372036854775807", "Wed, 21 Oct 2015 07:28:00 GMT", "1.5", "abc"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		d := ParseRetryAfter(value)
		if d < 0 {
			t.Fatalf("ParseRetryAfter(%q) = %v, a negative wait", value, d)
		}
		if secs, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && secs > 0 && secs <= maxRetryAfterSeconds {
			if d != time.Duration(secs)*time.Second {
				t.Fatalf("ParseRetryAfter(%q) = %v, want %ds", value, d, secs)
			}
		}
	})
}
