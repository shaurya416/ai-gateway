package client

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// maxExactSeconds is the largest delta-seconds value whose nanosecond count a
// float64 holds exactly.
const maxExactSeconds = (1 << 53) / int64(time.Second)

// FuzzParseRetryAfter holds the client's Retry-After reading to the header's
// two forms on any value: never a negative wait, and delta-seconds read as that
// many seconds.
func FuzzParseRetryAfter(f *testing.F) {
	for _, seed := range []string{"", "0", "1", "120", " 7 ", "-5", "9223372036854775807", "Wed, 21 Oct 2015 07:28:00 GMT", "1.5", "NaN", "Inf", "1e400"} {
		f.Add(seed)
	}
	now := time.Date(2015, 10, 21, 7, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, value string) {
		d := parseRetryAfter(value, now)
		if d < 0 {
			t.Fatalf("parseRetryAfter(%q) = %v, a negative wait", value, d)
		}
		if secs, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && secs > 0 && secs <= maxExactSeconds {
			if d != time.Duration(secs)*time.Second {
				t.Fatalf("parseRetryAfter(%q) = %v, want %ds", value, d, secs)
			}
		}
	})
}
