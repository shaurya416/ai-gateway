package providers

import (
	"reflect"
	"testing"
)

// TestSplitModels_TrimsEntries verifies a spaced or ragged comma-separated model
// list yields usable names: " b" would otherwise be registered as a model no
// request can ever match.
func TestSplitModels_TrimsEntries(t *testing.T) {
	tests := []struct {
		name string
		list string
		want []string
	}{
		{name: "spaced entries are trimmed", list: "a, b", want: []string{"a", "b"}},
		{name: "surrounding whitespace is trimmed", list: " a , b ", want: []string{"a", "b"}},
		{name: "empty entries are dropped", list: "a,,b,", want: []string{"a", "b"}},
		{name: "blank list yields nothing", list: " , ", want: nil},
		{name: "empty string yields nothing", list: "", want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := splitModels(tt.list); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("splitModels(%q) = %#v, want %#v", tt.list, got, tt.want)
			}
		})
	}
}

// TestOllamaBuild_TrimsConfiguredModels verifies the trimming reaches the built
// provider's model list, not just the parser.
func TestOllamaBuild_TrimsConfiguredModels(t *testing.T) {
	entry, ok := GetProviderEntry(NameOllama)
	if !ok {
		t.Fatal("ollama provider entry not found")
	}

	p, err := entry.Build(ProviderConfig{
		CfgKeyHost:   "http://localhost:11434",
		CfgKeyModels: "llama3.2, mistral",
	})
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	lister, ok := p.(interface{ ConfiguredModels() []string })
	if !ok {
		t.Fatal("ollama provider does not report supported models")
	}
	if want := []string{"llama3.2", "mistral"}; !reflect.DeepEqual(lister.ConfiguredModels(), want) {
		t.Errorf("ConfiguredModels() = %#v, want %#v", lister.ConfiguredModels(), want)
	}
}

// TestOllamaBuild_DropsDirectoryPathModelList is the regression test for the
// collision itself: a host running Ollama exports OLLAMA_MODELS as a directory,
// and that path used to be registered as one model id and advertised by
// /v1/models. The old name is no longer read, but the same path copied into
// FERRO_OLLAMA_MODELS must still be dropped, leaving the provider exactly as it
// is with no list configured.
func TestOllamaBuild_DropsDirectoryPathModelList(t *testing.T) {
	entry, ok := GetProviderEntry(NameOllama)
	if !ok {
		t.Fatal("ollama provider entry not found")
	}

	p, err := entry.Build(ProviderConfig{
		CfgKeyHost:   "http://localhost:11434",
		CfgKeyModels: "/root/.ollama/models",
	})
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	lister, ok := p.(interface{ ConfiguredModels() []string })
	if !ok {
		t.Fatal("ollama provider does not report supported models")
	}
	for _, m := range lister.ConfiguredModels() {
		if m == "/root/.ollama/models" {
			t.Fatalf("ConfiguredModels() = %#v: a filesystem path was registered as a model id",
				lister.ConfiguredModels())
		}
	}
}

// TestOllamaModelsEnv pins the rename's end state: FERRO_OLLAMA_MODELS is the
// model list, and OLLAMA_MODELS — Ollama's own models directory on any host
// running Ollama — is not read at all, so that directory never reaches it.
func TestOllamaModelsEnv(t *testing.T) {
	entry, ok := GetProviderEntry(NameOllama)
	if !ok {
		t.Fatal("ollama provider entry not found")
	}
	t.Setenv("OLLAMA_HOST", "http://localhost:11434")

	t.Run("FERRO_OLLAMA_MODELS read", func(t *testing.T) {
		t.Setenv("FERRO_OLLAMA_MODELS", "mistral")
		if got := ProviderConfigFromEnv(entry)[CfgKeyModels]; got != "mistral" {
			t.Errorf("models = %q, want %q", got, "mistral")
		}
	})

	for _, v := range []string{"/root/.ollama/models", "llama3.2"} {
		t.Run("OLLAMA_MODELS not read: "+v, func(t *testing.T) {
			t.Setenv("OLLAMA_MODELS", v)
			if got, set := ProviderConfigFromEnv(entry)[CfgKeyModels]; set {
				t.Errorf("models = %q: OLLAMA_MODELS reached the model list", got)
			}
		})
	}
}
