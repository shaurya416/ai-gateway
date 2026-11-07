package envref

import (
	"strings"
	"testing"
)

// FuzzExpand holds Expand to its godoc on any value: text with no ${VAR}
// reference comes back byte for byte, a bare "$" included, and a reference to
// a defined variable is replaced by its value and nothing else, so a value
// holding "${...}" is not expanded a second time.
func FuzzExpand(f *testing.F) {
	for _, seed := range []string{"", "costs $5", "pa$$w0rd", "${FUZZ_VALUE}", "a${FUZZ_VALUE}b${FUZZ_VALUE}", "${FUZZ_UNSET_X}", "$${FUZZ_VALUE}", "${", "${1A}"} {
		f.Add(seed, "v")
		f.Add(seed, "${FUZZ_VALUE}")
	}
	f.Fuzz(func(t *testing.T, s, value string) {
		if strings.ContainsRune(value, 0) {
			return
		}
		t.Setenv("FUZZ_VALUE", value)
		got, err := Expand(s)
		if !HasReference(s) {
			if err != nil || got != s {
				t.Fatalf("Expand(%q) = %q, %v; want it unchanged", s, got, err)
			}
			return
		}
		for _, ref := range refPattern.FindAllStringSubmatch(s, -1) {
			if ref[1] != "FUZZ_VALUE" {
				return
			}
		}
		if err != nil {
			t.Fatalf("Expand(%q) = %v with every reference defined", s, err)
		}
		{
			want := refPattern.ReplaceAllStringFunc(s, func(string) string { return value })
			if got != want {
				t.Fatalf("Expand(%q) = %q, want %q", s, got, want)
			}
		}
	})
}
