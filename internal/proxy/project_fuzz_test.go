package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// jsonStringValues returns every string value in a valid JSON document, keys
// excluded, read token by token so a value is counted wherever it appears.
func jsonStringValues(t *testing.T, doc []byte) []string {
	t.Helper()
	type frame struct{ object, nextIsKey bool }
	var (
		values []string
		stack  []frame
	)
	valueDone := func() {
		if n := len(stack); n > 0 && stack[n-1].object {
			stack[n-1].nextIsKey = true
		}
	}
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return values
		}
		if err != nil {
			t.Fatalf("token: %v", err)
		}
		switch tok {
		case json.Delim('{'):
			stack = append(stack, frame{object: true, nextIsKey: true})
			continue
		case json.Delim('['):
			stack = append(stack, frame{})
			continue
		case json.Delim('}'), json.Delim(']'):
			stack = stack[:len(stack)-1]
			valueDone()
			continue
		}
		if n := len(stack); n > 0 && stack[n-1].object && stack[n-1].nextIsKey {
			stack[n-1].nextIsKey = false
			continue
		}
		if s, ok := tok.(string); ok {
			values = append(values, s)
		}
		valueDone()
	}
}

// FuzzProjectBody holds the projection to its godoc on any request body: a
// JSON body within the cap is inspectable and its projection holds every
// string value in it, at any depth; anything else is uninspectable; and the
// body forwarded afterwards is the body that arrived.
func FuzzProjectBody(f *testing.F) {
	f.Add([]byte(`{"model":"gpt-4o","input":"hello","tools":[{"name":"t","description":"d"}]}`))
	f.Add([]byte(`{"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`))
	f.Add([]byte(`["a",{"b":"c"},1,true,null]`))
	f.Add([]byte(`{"a":"x"} trailing`))
	f.Add([]byte("--boundary\r\nContent-Disposition: form-data"))
	f.Add([]byte(``))

	f.Fuzz(func(t *testing.T, body []byte) {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://gateway/v1/responses", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		text, _, inspectable := projectBody(r)
		forwarded, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(forwarded, body) {
			t.Fatalf("the projection did not restore the body")
		}
		if len(body) == 0 {
			if !inspectable || text != "" {
				t.Fatalf("an empty body projected to %q, inspectable=%v", text, inspectable)
			}
			return
		}
		valid := json.Valid(body) && len(body) <= projectionCap
		if inspectable != valid {
			t.Fatalf("inspectable = %v for a body whose validity is %v", inspectable, valid)
		}
		if !valid {
			return
		}
		for _, value := range jsonStringValues(t, body) {
			if !strings.Contains(text, value) {
				t.Fatalf("the projection %q omits the string value %q", text, value)
			}
		}
	})
}
