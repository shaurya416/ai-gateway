package client_test

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	aigateway "github.com/ferro-labs/ai-gateway"
	"github.com/ferro-labs/ai-gateway/client"
	"github.com/ferro-labs/ai-gateway/config"
)

const (
	exampleChatModel  = "gpt-4o-mini"
	exampleEmbedModel = "text-embedding-3-small"
)

var (
	exampleOnce sync.Once
	exampleURL  string
)

// exampleGatewayURL serves the gateway's own handlers over a stub provider
// for the examples to call: two targets, "fast" and "smart", with a
// conditional rule sending X-Gateway-Metadata tier=gold to "smart".
func exampleGatewayURL() string {
	exampleOnce.Do(func() {
		gw, err := aigateway.New(config.Config{
			Strategy: config.StrategyConfig{
				Mode: config.ModeConditional,
				Conditions: []config.Condition{
					{Key: config.ConditionKeyMetadata, Field: "tier", Value: "gold", TargetKey: "smart"},
				},
			},
			Targets: []config.Target{
				{VirtualKey: "fast", Models: []string{exampleChatModel, exampleEmbedModel, throttledModel}},
				{VirtualKey: "smart", Models: []string{exampleChatModel}},
			},
		})
		if err != nil {
			panic(err)
		}
		gw.RegisterProviderAs("fast", stubProvider{})
		gw.RegisterProviderAs("smart", stubProvider{})
		exampleURL = httptest.NewServer(gatewayMux(gw)).URL
	})
	return exampleURL
}

func Example() {
	c, err := client.New(exampleGatewayURL(), "sk-your-gateway-key")
	if err != nil {
		panic(err)
	}

	resp, err := c.ChatCompletion(context.Background(), client.ChatCompletionRequest{
		Model: "gpt-4o-mini",
		Messages: []client.Message{
			client.SystemMessage("Be brief."),
			client.UserMessage("Hello"),
		},
		Temperature: client.Ptr(0.2),
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(resp.Choices[0].Message.Content)

	// Every routed response names the target that served it.
	a := resp.Meta.Attribution
	fmt.Printf("target=%s provider=%s model=%s attempts=%d\n", a.Target, a.Provider, a.Model, a.Attempts)
	// Output:
	// echo: Hello
	// target=fast provider=stubvendor model=gpt-4o-mini attempts=1
}

func ExampleClient_ChatCompletionStream() {
	c, err := client.New(exampleGatewayURL(), "sk-your-gateway-key")
	if err != nil {
		panic(err)
	}

	stream, err := c.ChatCompletionStream(context.Background(), client.ChatCompletionRequest{
		Model:    "gpt-4o-mini",
		Messages: []client.Message{client.UserMessage("Stream this")},
	})
	if err != nil {
		panic(err) // the gateway refused before the first chunk: an *client.APIError
	}
	defer func() { _ = stream.Close() }()

	// The routing decision is made before the first chunk is written.
	fmt.Println("served by", stream.Meta().Attribution.Target)

	var text strings.Builder
	var acc client.ChatCompletionAccumulator
	for chunk, err := range stream.Chunks() {
		if err != nil {
			panic(err) // a failure inside the stream: an *client.StreamError, a context error, …
		}
		acc.Add(chunk)
		for _, choice := range chunk.Choices {
			text.WriteString(choice.Delta.Content)
		}
	}
	fmt.Println(text.String())

	completion := acc.ChatCompletion()
	fmt.Println("finish reason:", completion.Choices[0].FinishReason)
	// Output:
	// served by fast
	// echo: Stream this
	// finish reason: stop
}

func ExampleChatCompletionStream_Next() {
	c, err := client.New(exampleGatewayURL(), "sk-your-gateway-key")
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	stream, err := c.ChatCompletionStream(ctx, client.ChatCompletionRequest{
		Model:    "gpt-4o-mini",
		Messages: []client.Message{client.UserMessage("one chunk at a time")},
	})
	if err != nil {
		panic(err)
	}
	defer func() { _ = stream.Close() }()

	for stream.Next() {
		for _, choice := range stream.Current().Choices {
			fmt.Printf("%q\n", choice.Delta.Content)
		}
	}
	if err := stream.Err(); err != nil {
		panic(err)
	}
	// Output:
	// "echo"
	// ": "
	// "one chunk at a time"
	// ""
}

func ExampleWithRoutingMetadata() {
	c, err := client.New(exampleGatewayURL(), "sk-your-gateway-key")
	if err != nil {
		panic(err)
	}
	req := client.ChatCompletionRequest{
		Model:    "gpt-4o-mini",
		Messages: []client.Message{client.UserMessage("Hello")},
	}

	// The example gateway routes metadata tier=gold to the "smart" target.
	resp, err := c.ChatCompletion(context.Background(), req,
		client.WithRoutingMetadata(map[string]any{"tier": "gold", "priority": 2}))
	if err != nil {
		panic(err)
	}
	fmt.Println(resp.Meta.Attribution.Target)

	resp, err = c.ChatCompletion(context.Background(), req)
	if err != nil {
		panic(err)
	}
	fmt.Println(resp.Meta.Attribution.Target)
	// Output:
	// smart
	// fast
}

func ExampleEncodeRoutingMetadata() {
	value, err := client.EncodeRoutingMetadata(map[string]any{"tier": "gold", "region": "Zürich", "beta": true})
	if err != nil {
		panic(err)
	}
	fmt.Println(value)

	_, err = client.EncodeRoutingMetadata(map[string]any{"filters": []string{"a", "b"}})
	fmt.Println(errors.Is(err, client.ErrInvalidRoutingMetadata))
	// Output:
	// {"beta":true,"region":"Z\u00fcrich","tier":"gold"}
	// true
}

func ExampleAPIError() {
	c, err := client.New(exampleGatewayURL(), "sk-your-gateway-key")
	if err != nil {
		panic(err)
	}

	// The example gateway's upstream answers this model with a 429 and
	// Retry-After: 3.
	_, err = c.ChatCompletion(context.Background(), client.ChatCompletionRequest{
		Model:    "throttled",
		Messages: []client.Message{client.UserMessage("Hello")},
	})

	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		fmt.Println(apiErr.StatusCode, apiErr.Code)
		fmt.Println("retryable:", apiErr.Retryable(), "after:", apiErr.RetryAfter)
		fmt.Println("refused by:", apiErr.Meta.Attribution.Target)
	}
	// Output:
	// 429 rate_limit_exceeded
	// retryable: true after: 3s
	// refused by: fast
}

func ExampleClient_Embeddings() {
	c, err := client.New(exampleGatewayURL(), "sk-your-gateway-key")
	if err != nil {
		panic(err)
	}

	resp, err := c.Embeddings(context.Background(), client.EmbeddingRequest{
		Model: "text-embedding-3-small",
		Input: []string{"first", "second document"},
	})
	if err != nil {
		panic(err)
	}
	for _, e := range resp.Data {
		fmt.Println(e.Index, len(e.Embedding))
	}
	// Output:
	// 0 2
	// 1 2
}

func ExampleClient_Models() {
	c, err := client.New(exampleGatewayURL(), "sk-your-gateway-key")
	if err != nil {
		panic(err)
	}

	models, err := c.Models(context.Background())
	if err != nil {
		panic(err)
	}
	for _, m := range models.Data {
		if m.ID == "gpt-4o-mini" {
			fmt.Println(m.ID, "is advertised")
		}
	}
	// Output:
	// gpt-4o-mini is advertised
}

func ExampleClient_Capabilities() {
	c, err := client.New(exampleGatewayURL(), "sk-your-gateway-key")
	if err != nil {
		panic(err)
	}

	caps, err := c.Capabilities(context.Background())
	if err != nil {
		panic(err)
	}
	support, ok := caps.Support("fast", "temperature")
	fmt.Println(support, ok)
	// Output:
	// forward true
}
