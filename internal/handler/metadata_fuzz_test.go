package handler

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/textproto"
	"testing"

	"github.com/ferro-labs/ai-gateway/client"
)

// FuzzRoutingMetadataFromClient holds the Go client and the gateway to one
// contract for X-Gateway-Metadata: a value EncodeRoutingMetadata produces
// crosses an HTTP hop intact and the gateway reads back every entry, each as
// the text a rule matches. One the client refuses is one it says the gateway
// would refuse, and nothing the gateway reads makes it fail.
func FuzzRoutingMetadataFromClient(f *testing.F) {
	f.Add("tier", "gold", "priority", 42.0, true)
	f.Add("tier", "gölд", "", -0.5, false)
	f.Add("k ", "<&>\x7f", "e", 1e21, true)

	f.Fuzz(func(t *testing.T, k1, v1, k2 string, num float64, flag bool) {
		md := map[string]any{k1: v1, k2: num, k1 + "!": flag}
		encoded, err := client.EncodeRoutingMetadata(md)
		if err != nil {
			return
		}
		sent := http.Header{}
		sent.Set(HeaderRoutingMetadata, encoded)
		var wire bytes.Buffer
		if err := sent.Write(&wire); err != nil {
			t.Fatalf("header does not serialise: %v", err)
		}
		wire.WriteString("\r\n")
		mime, err := textproto.NewReader(bufio.NewReader(&wire)).ReadMIMEHeader()
		if err != nil {
			t.Fatalf("header does not parse back: %v", err)
		}
		got, err := routingMetadata(http.Header(mime))
		if err != nil {
			t.Fatalf("gateway refused %q, which the client encoded: %v", encoded, err)
		}
		if len(got) != len(md) {
			t.Fatalf("gateway read %d entries of %d: %v", len(got), len(md), got)
		}
		for key, value := range md {
			var want string
			if s, ok := value.(string); ok {
				want = s
			} else {
				raw, _ := json.Marshal(value)
				want = string(raw)
			}
			if got[key] != want {
				t.Fatalf("entry %q reads %q, want %q (header %q)", key, got[key], want, encoded)
			}
		}
	})
}
