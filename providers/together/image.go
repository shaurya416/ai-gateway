package together

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// togetherImageRequest is the image-generation body Together AI accepts on
// /images/generations (FLUX models). It is OpenAI-shaped except where
// Together's schema differs: the dimensions travel as width and height — there
// is no size field — and response_format is "base64" or "url".
type togetherImageRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              *int   `json:"n,omitempty"`
	Width          int    `json:"width,omitempty"`
	Height         int    `json:"height,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
	User           string `json:"user,omitempty"`
}

// togetherFormatBase64 is Together's response_format for what OpenAI calls
// b64_json; the response still carries the image under b64_json.
const togetherFormatBase64 = "base64"

// imageDimensions reads OpenAI's "WIDTHxHEIGHT" size as the width and height
// Together takes. An unset size and OpenAI's "auto" leave both to the model's
// default. Any other value that is not two positive integers is the caller's
// 400, refused before the upstream call rather than answered with an image of
// a size nobody asked for.
func imageDimensions(size string) (width, height int, err error) {
	if size == "" || size == "auto" {
		return 0, 0, nil
	}
	w, h, ok := strings.Cut(size, "x")
	width, errW := strconv.Atoi(w)
	height, errH := strconv.Atoi(h)
	if !ok || errW != nil || errH != nil || width <= 0 || height <= 0 {
		return 0, 0, core.StatusError(Name, http.StatusBadRequest,
			fmt.Sprintf("invalid size %q: expected WxH format with positive integers (e.g. \"1024x1024\")", size))
	}
	return width, height, nil
}

// GenerateImage sends an image generation request to Together AI's
// /images/generations endpoint (FLUX), translating the OpenAI size and
// response_format onto Together's own fields.
func (p *Provider) GenerateImage(ctx context.Context, req core.ImageRequest) (*core.ImageResponse, error) {
	if err := core.EnforceImageResponseFormat(p.name, req); err != nil {
		return nil, err
	}

	width, height, err := imageDimensions(req.Size)
	if err != nil {
		return nil, err
	}
	format := req.ResponseFormat
	if format == core.ImageResponseFormatB64JSON {
		format = togetherFormatBase64
	}

	payload := togetherImageRequest{
		Model:          req.Model,
		Prompt:         req.Prompt,
		N:              req.N,
		Width:          width,
		Height:         height,
		ResponseFormat: format,
		User:           req.User,
	}

	body, contentLen, release, err := core.JSONBodyReader(payload)
	if err != nil {
		return nil, fmt.Errorf("together: failed to marshal image request: %w", err)
	}
	defer release()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/images/generations", body)
	if err != nil {
		return nil, fmt.Errorf("together: failed to create image request: %w", err)
	}
	httpReq.ContentLength = int64(contentLen)
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("together: image request failed: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	respBody, err := core.ReadResponseBody(httpResp.Body, core.MaxProviderResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("together: failed to read image response: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		return nil, core.APIErrorFromResponse("together", httpResp, respBody)
	}

	var decoded core.ImageResponse
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return nil, fmt.Errorf("together: failed to decode image response: %w", err)
	}
	if err := core.RequireGeneratedImage("together", decoded.Data); err != nil {
		return nil, err
	}

	if decoded.Created == 0 {
		decoded.Created = time.Now().Unix()
	}

	return &decoded, nil
}
