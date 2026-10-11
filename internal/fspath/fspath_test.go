package fspath

import "testing"

// TestIsDirectoryPath guards the Ollama models-directory collision: Ollama's own
// OLLAMA_MODELS is its models *directory*, so a path landing in a model list
// must be recognised — without swallowing model ids that merely contain a colon
// or a slash.
func TestIsDirectoryPath(t *testing.T) {
	paths := []string{
		"/root/.ollama/models", // Ollama's own default shape
		"/usr/share/ollama/.ollama/models",
		"~/.ollama/models",
		"./models",
		"../models",
		`C:\Users\me\.ollama\models`,
		"D:/ollama/models",
	}
	for _, v := range paths {
		if !IsDirectoryPath(v) {
			t.Errorf("IsDirectoryPath(%q) = false, want true", v)
		}
	}

	modelLists := []string{
		"", "llama3.2", "gpt-oss:20b", "llama3.2, mistral",
		"library/mistral", "meta/llama-2-70b-chat",
		"deepseek-r1:8b,qwen2.5:14b",
		"/root/.ollama/models,llama3.2", // a comma means someone meant a list
	}
	for _, v := range modelLists {
		if IsDirectoryPath(v) {
			t.Errorf("IsDirectoryPath(%q) = true, want false", v)
		}
	}
}
