package cache

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers"
)

// FuzzFingerprintStable holds the cache key to being a function of the
// request alone: the same request decoded twice keys the same, and a request
// that has been through the gateway's own wire encoding and back keys as it did
// before, so a replica that re-encodes a request cannot miss its own entry.
func FuzzFingerprintStable(f *testing.F) {
	f.Add(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"temperature":0.2,"tool_choice":{"type":"function","function":{"name":"f"}}}`, "key-1")
	f.Add(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}],"logit_bias":{"1":-100,"2":5}}`, "")
	f.Add(`{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]}],"stop":["x"]}`, "ns")
	f.Fuzz(func(t *testing.T, body, namespace string) {
		var first, second providers.Request
		if json.Unmarshal([]byte(body), &first) != nil || json.Unmarshal([]byte(body), &second) != nil {
			return
		}
		key, err := Fingerprint(&first, namespace)
		if err != nil {
			return
		}
		if !strings.HasPrefix(key, "v1:") || len(key) != 3+64 {
			t.Fatalf("key %q is not a v1 fingerprint", key)
		}
		if again, _ := Fingerprint(&second, namespace); again != key {
			t.Fatalf("the same body keyed %q and %q", key, again)
		}
		encoded, err := json.Marshal(first)
		if err != nil {
			return
		}
		var third providers.Request
		if err := json.Unmarshal(encoded, &third); err != nil {
			t.Fatalf("a request's own encoding does not decode: %v\n%s", err, encoded)
		}
		if roundTrip, _ := Fingerprint(&third, namespace); roundTrip != key {
			t.Fatalf("re-encoding changed the key\n body: %s\n again: %s", body, encoded)
		}
	})
}
