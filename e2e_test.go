package agent

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestE2ESimpleResponsesCall(t *testing.T) {
	if os.Getenv("OPENROUTER_API_KEY") == "" {
		t.Skip("OPENROUTER_API_KEY is not set")
	}
	if os.Getenv("OPENROUTER_AGENT_E2E") != "1" {
		t.Skip("set OPENROUTER_AGENT_E2E=1 to run live OpenRouter e2e")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	client := NewOpenRouter(OpenRouterOptions{})
	result, err := CallModel(ctx, client, CallModelInput{
		Model: "openai/gpt-4o-mini",
		Input: "Reply with exactly: OK",
	})
	if err != nil {
		t.Fatal(err)
	}
	text, err := result.Text(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToUpper(text), "OK") {
		t.Fatalf("expected OK in response, got %q", text)
	}
}
