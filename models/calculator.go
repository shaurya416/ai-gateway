package models

// Usage carries all token and media counts from a completed provider response.
// This is intentionally a separate type from providers.Usage so the models
// package has no dependency on the providers package and can be imported
// independently.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	ReasoningTokens  int     // subset of CompletionTokens; billed at its own rate when the row has one
	CacheReadTokens  int     // prompt cache hits (cheaper)
	CacheWriteTokens int     // prompt cache misses, written to cache
	ImageCount       int     // image generation requests
	AudioInputSecs   float64 // audio transcription (Whisper)
	AudioOutputChars int     // TTS (character count)
}

// CostResult breaks down the total cost by billing component.
// Every field is in USD.
type CostResult struct {
	TotalUSD      float64
	InputUSD      float64
	OutputUSD     float64
	CacheReadUSD  float64
	CacheWriteUSD float64
	ReasoningUSD  float64
	ImageUSD      float64
	AudioUSD      float64
	EmbeddingUSD  float64
	// ModelFound is false when the catalog has no entry for the requested model.
	// All cost fields will be zero in that case.
	ModelFound bool
	// Priced is false when the catalog entry carries no price for the field its
	// mode bills off: input tokens for chat and responses, embedding tokens for
	// embeddings, per-tile or (failing that, and only against reported usage)
	// per-token for images, per-minute or per-character for audio. Every rate
	// but the per-tile one prices only a reported measurement — token usage, a
	// duration or a character count — so a request that reported none is
	// unpriced rather than a priced $0.00. An image priced per token is priced
	// only when the row carries a rate for every token count reported.
	Priced bool
}

// reportsTokens reports whether the usage carries any token count at all.
func (u Usage) reportsTokens() bool {
	return u.PromptTokens > 0 || u.CompletionTokens > 0 || u.CacheReadTokens > 0 ||
		u.CacheWriteTokens > 0 || u.ReasoningTokens > 0
}

// perM converts a nullable price-per-million-tokens to a cost for n tokens.
// Returns 0 when price is nil (field not applicable) or n is 0.
func perM(price *float64, n int) float64 {
	if price == nil || n == 0 {
		return 0
	}
	return *price * float64(n) / 1_000_000
}

// Calculate computes the full cost for a completed request.
// modelKey should be "provider/model-id"; a bare model ID is also accepted
// and resolved via the reverse index built at catalog load time. A
// "provider/model-id" key absent from the provider's rows resolves to another
// row only when that row is the one carrying "provider/model-id" as its model
// ID; when several rows carry it, the model is unpriced.
func Calculate(catalog Catalog, modelKey string, usage Usage) CostResult {
	model, ok := catalog.GetForPricing(modelKey)
	if !ok {
		return CostResult{ModelFound: false}
	}

	p := model.Pricing
	r := CostResult{ModelFound: true}

	// Priced is per MODE, because each mode bills off a different price field.
	// Reading InputPerMTokens for all of them reported every embedding and every
	// image model unpriced however complete its catalog entry was — and
	// cost-optimized routing skips unpriced candidates, so under
	// `unpriced_strategy: skip` no embedding target could rank at all.
	switch model.Mode {
	// A responses-mode row is a model served only on the Responses API, priced
	// with the same per-token rates under the same inclusive-prompt convention.
	// Without this arm every /v1/responses request to one was recorded unpriced.
	case ModeChat, ModeResponses:
		// Priced only against reported usage, for the reason the image token arm
		// and the audio arms are: a completion has at least one prompt token, so
		// a usage block with no token count at all is one the provider did not
		// send — a Replicate model that reports no metrics, a stream whose
		// upstream ignored include_usage — and pricing it recorded a priced
		// $0.00 as though it were the real figure.
		r.Priced = p.InputPerMTokens != nil && usage.reportsTokens()
		// PromptTokens is INCLUSIVE of CacheReadTokens on every provider: that
		// is the OpenAI convention, and providers/core/anthropicwire folds
		// Anthropic's exclusive count into it at decode so exactly one
		// convention reaches here. So the cached subset must come off the
		// input-rate count, or it is paid for twice — once at the full input
		// rate inside PromptTokens and again at the cache rate below.
		//
		// With no cache rate in the catalog there is no discount to apply, so
		// the cached subset stays on the input rate rather than becoming free:
		// an unknown rate must not silently bill as zero.
		promptBillable := usage.PromptTokens
		if p.CacheReadPerMTokens != nil {
			// Clamped: a provider reporting more cached than prompt tokens
			// must not produce a negative billable count.
			promptBillable = max(usage.PromptTokens-usage.CacheReadTokens, 0)
		}
		// CompletionTokens is likewise INCLUSIVE of ReasoningTokens (the OpenAI
		// convention; xAI's and gemini's disjoint counts are folded into it at
		// decode), so a reasoning rate takes the reasoning subset off the
		// output-rate count by the same rule: added on top, every reasoning
		// token was billed twice. With no reasoning rate the subset stays on
		// the output rate, which is how it is billed.
		completionBillable := usage.CompletionTokens
		if p.ReasoningPerMTokens != nil {
			completionBillable = max(usage.CompletionTokens-usage.ReasoningTokens, 0)
		}
		r.InputUSD = perM(p.InputPerMTokens, promptBillable)
		r.OutputUSD = perM(p.OutputPerMTokens, completionBillable)
		r.CacheReadUSD = perM(p.CacheReadPerMTokens, usage.CacheReadTokens)
		r.CacheWriteUSD = perM(p.CacheWritePerMTokens, usage.CacheWriteTokens)
		r.ReasoningUSD = perM(p.ReasoningPerMTokens, usage.ReasoningTokens)

	case ModeEmbedding:
		// Embedded input is never empty, so no input-token count means none was
		// reported — every Gemini embedding, whose endpoint sends no usage.
		r.Priced = p.EmbeddingPerMTokens != nil && usage.PromptTokens > 0
		r.EmbeddingUSD = perM(p.EmbeddingPerMTokens, usage.PromptTokens)

	case ModeImage:
		// Two billing shapes live under this one mode. Most image models carry a
		// per-tile price; the gpt-image family and fireworks' image models carry
		// none and are billed per token instead. Per-tile wins wherever both are
		// present — the gemini and vertex-ai image rows carry both — because
		// per-image is what those providers actually bill.
		//
		// The token arm requires reported usage, and that guard is the point of
		// it. Every image provider but OpenAI and Azure OpenAI reports none, and
		// pricing a zero count would record $0.00 as though it were the real
		// figure while flagging the request priced: the known-zero this
		// mode-aware flag exists to prevent. No usage means unpriced, which is
		// the true answer.
		//
		// Reported usage is priced only when the row carries a rate for every
		// count it reports. The gpt-image-1 and gpt-image-1-mini rows carry an
		// input rate and no output rate, and billing the image's output tokens
		// at nothing recorded a generation at the cost of its prompt alone —
		// a fraction of a cent — while flagging that figure complete.
		switch {
		case p.ImagePerTile != nil:
			r.Priced = true
			if usage.ImageCount > 0 {
				r.ImageUSD = *p.ImagePerTile * float64(usage.ImageCount)
			}
		case usage.PromptTokens > 0 || usage.CompletionTokens > 0:
			// A nil rate is unknown, not free (see Pricing).
			r.Priced = (usage.PromptTokens == 0 || p.InputPerMTokens != nil) &&
				(usage.CompletionTokens == 0 || p.OutputPerMTokens != nil)
			r.InputUSD = perM(p.InputPerMTokens, usage.PromptTokens)
			r.OutputUSD = perM(p.OutputPerMTokens, usage.CompletionTokens)
		}

	// The audio modes bill off a measurement — seconds of input, characters of
	// output — and require one, for the reason the image token arm does: the
	// transcription and speech surfaces report neither, so pricing the absent
	// figure recorded every whisper-1 transcription as a priced $0.00. No
	// measurement means unpriced.
	case ModeAudioIn:
		if p.AudioInputPerMinute != nil && usage.AudioInputSecs > 0 {
			r.Priced = true
			r.AudioUSD = *p.AudioInputPerMinute * usage.AudioInputSecs / 60
		}

	case ModeAudioOut:
		if p.AudioOutputPerCharacter != nil && usage.AudioOutputChars > 0 {
			r.Priced = true
			r.AudioUSD = *p.AudioOutputPerCharacter * float64(usage.AudioOutputChars)
		}
	}

	r.TotalUSD = r.InputUSD + r.OutputUSD + r.CacheReadUSD +
		r.CacheWriteUSD + r.ReasoningUSD + r.ImageUSD + r.AudioUSD + r.EmbeddingUSD
	return r
}
