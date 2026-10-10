package gemini

import (
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// cumulativeUsageStream is a recorded streamGenerateContent?alt=sse response
// (gemini-3-pro-preview), abridged — thoughtSignature, promptTokensDetails and
// the tail of the second chunk's text removed: Gemini repeats usageMetadata on
// EVERY chunk, as a running total, not on the last one only.
const cumulativeUsageStream = `data: {"candidates":[{"content":{"parts":[{"text":"There are **3**"}],"role":"model"},"index":0}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":5,"totalTokenCount":199,"thoughtsTokenCount":185},"modelVersion":"gemini-3-pro-preview","responseId":"bH6LaZW8Fp_3nsEPqtaSwQ4"}

data: {"candidates":[{"content":{"parts":[{"text":" \"r\"s in strawberry."}],"role":"model"},"index":0}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":23,"totalTokenCount":217,"thoughtsTokenCount":185},"modelVersion":"gemini-3-pro-preview","responseId":"bH6LaZW8Fp_3nsEPqtaSwQ4"}

data: {"candidates":[{"content":{"parts":[{"text":""}],"role":"model"},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":23,"totalTokenCount":217,"thoughtsTokenCount":185},"modelVersion":"gemini-3-pro-preview","responseId":"bH6LaZW8Fp_3nsEPqtaSwQ4"}

`

// TestGeminiProvider_CompleteStream_UsageOnceAtTheFinish pins that a stream
// reports usage once, on the chunk that finishes it, whatever Gemini repeats
// along the way. Forwarding each running total put a usage block on every
// chunk; the OpenAI streaming contract has usage on the final chunk only, and a
// client that adds up the usage it is handed — LangChain's ChatOpenAI does —
// counted this three-chunk answer as 27 prompt tokens and 633 in total instead
// of 9 and 217.
func TestGeminiProvider_CompleteStream_UsageOnceAtTheFinish(t *testing.T) {
	chunks := streamGemini(t, cumulativeUsageStream)

	finishIdx := -1
	var usageIdx []int
	var content string
	for i, c := range chunks {
		if c.Error != nil {
			t.Fatalf("chunk %d error = %v", i, c.Error)
		}
		for _, choice := range c.Choices {
			content += choice.Delta.Content
			if choice.FinishReason != "" {
				finishIdx = i
			}
		}
		if c.Usage != nil {
			usageIdx = append(usageIdx, i)
		}
	}
	if content != `There are **3** "r"s in strawberry.` {
		t.Errorf("content = %q", content)
	}
	if len(usageIdx) != 1 {
		t.Fatalf("usage carried on chunks %v of %d; want exactly one", usageIdx, len(chunks))
	}
	if usageIdx[0] < finishIdx {
		t.Errorf("usage on chunk %d precedes the finish_reason chunk %d", usageIdx[0], finishIdx)
	}
	u := chunks[usageIdx[0]].Usage
	if u.PromptTokens != 9 || u.CompletionTokens != 208 || u.TotalTokens != 217 || u.ReasoningTokens != 185 {
		t.Errorf("usage = %+v, want the final running total 9/208/217 with 185 reasoning", u)
	}
}

// TestGeminiProvider_CompleteStream_UsageWithoutFinish pins that a stream which
// ends cleanly without a finishing candidate still reports its usage once, so
// holding usage for the finish never loses it.
func TestGeminiProvider_CompleteStream_UsageWithoutFinish(t *testing.T) {
	const sse = `data: {"candidates":[{"content":{"parts":[{"text":"partial"}],"role":"model"}}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":1,"totalTokenCount":5},"responseId":"r1"}` + "\n\n" +
		`data: {"candidates":[{"content":{"parts":[{"text":" answer"}],"role":"model"}}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2,"totalTokenCount":6},"responseId":"r1"}` + "\n\n"

	chunks := streamGemini(t, sse)

	var usages []*core.Usage
	for i, c := range chunks {
		if c.Error != nil {
			t.Fatalf("chunk %d error = %v", i, c.Error)
		}
		if c.Usage != nil {
			usages = append(usages, c.Usage)
		}
	}
	if len(usages) != 1 {
		t.Fatalf("usage reported %d times; want once", len(usages))
	}
	if last := chunks[len(chunks)-1]; last.Usage == nil {
		t.Errorf("usage is not on the last chunk")
	}
	if u := usages[0]; u.PromptTokens != 4 || u.CompletionTokens != 2 || u.TotalTokens != 6 {
		t.Errorf("usage = %+v, want the final running total 4/2/6", u)
	}
}
