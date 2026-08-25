package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"runtime/debug"
	"strings"
	"testing"
)

type fakeHTTPClient struct{}

func (fakeHTTPClient) Do(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: req}, nil
}

func TestMiddlewareClientFiresHooks(t *testing.T) {
	var before, after bool
	client := &middlewareClient{base: fakeHTTPClient{}, before: []RequestMiddleware{func(ctx context.Context, req *http.Request) error {
		before = true
		req.Header.Set("x-test", "1")
		return nil
	}}, after: []ResponseMiddleware{func(ctx context.Context, res *http.Response) error { after = true; return nil }}}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.test", nil)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || !before || !after {
		t.Fatalf("middleware did not fire")
	}
	if req.Header.Get("x-test") != "1" {
		t.Fatalf("before middleware did not mutate request")
	}
}

func TestMiddlewareClientAppendsAgentUserAgent(t *testing.T) {
	client := &middlewareClient{
		base:       fakeHTTPClient{},
		agentToken: "github.com/OpenRouterTeam/go-agent/0.8.0",
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.test", nil)
	req.Header.Set("User-Agent", "speakeasy-sdk/go 0.7.82 2.914.0 1.0.0 github.com/OpenRouterTeam/go-sdk")

	if _, err := client.Do(req); err != nil {
		t.Fatal(err)
	}

	want := "speakeasy-sdk/go 0.7.82 2.914.0 1.0.0 github.com/OpenRouterTeam/go-sdk github.com/OpenRouterTeam/go-agent/0.8.0"
	if got := req.Header.Get("User-Agent"); got != want {
		t.Fatalf("User-Agent = %q, want %q", got, want)
	}
}

func TestMiddlewareClientDoesNotDuplicateAgentUserAgent(t *testing.T) {
	client := &middlewareClient{
		base:       fakeHTTPClient{},
		agentToken: "github.com/OpenRouterTeam/go-agent/0.8.0",
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.test", nil)
	req.Header.Set("User-Agent", "speakeasy-sdk/go github.com/OpenRouterTeam/go-agent/0.8.0")

	if _, err := client.Do(req); err != nil {
		t.Fatal(err)
	}
	first := req.Header.Get("User-Agent")
	if _, err := client.Do(req); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("User-Agent"); got != first {
		t.Fatalf("User-Agent changed on retry: first %q, second %q", first, got)
	}
}

func TestMiddlewareClientAllowsCallerMiddlewareToOverrideAgentUserAgent(t *testing.T) {
	client := &middlewareClient{
		base:       fakeHTTPClient{},
		agentToken: "github.com/OpenRouterTeam/go-agent/0.8.0",
		before: []RequestMiddleware{func(_ context.Context, req *http.Request) error {
			req.Header.Set("User-Agent", "caller/1.0")
			return nil
		}},
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.test", nil)

	if _, err := client.Do(req); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("User-Agent"); got != "caller/1.0" {
		t.Fatalf("User-Agent = %q, want caller override", got)
	}
}

func TestAgentUserAgentTokenFromBuildInfo(t *testing.T) {
	tests := []struct {
		name      string
		buildInfo *debug.BuildInfo
		want      string
	}{
		{
			name: "main module version",
			buildInfo: &debug.BuildInfo{
				Main: debug.Module{Path: agentModulePath, Version: "v0.8.0"},
			},
			want: "github.com/OpenRouterTeam/go-agent/0.8.0",
		},
		{
			name: "dependency version",
			buildInfo: &debug.BuildInfo{
				Main: debug.Module{Path: "example.com/app", Version: "v1.0.0"},
				Deps: []*debug.Module{{Path: agentModulePath, Version: "v0.8.1"}},
			},
			want: "github.com/OpenRouterTeam/go-agent/0.8.1",
		},
		{
			name: "development version",
			buildInfo: &debug.BuildInfo{
				Main: debug.Module{Path: agentModulePath, Version: "(devel)"},
			},
			want: agentModulePath,
		},
		{
			name: "missing version",
			buildInfo: &debug.BuildInfo{
				Main: debug.Module{Path: agentModulePath},
			},
			want: agentModulePath,
		},
		{
			name: "module not found",
			buildInfo: &debug.BuildInfo{
				Main: debug.Module{Path: "example.com/app", Version: "v1.0.0"},
			},
			want: agentModulePath,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := agentUserAgentTokenFromBuildInfo(test.buildInfo); got != test.want {
				t.Fatalf("agentUserAgentTokenFromBuildInfo() = %q, want %q", got, test.want)
			}
		})
	}
}

/*
 * recordingHTTPClient captures the request as it reaches the transport, i.e.
 * after the whole NewOpenRouter middleware chain has run. fakeHTTPClient
 * cannot stand in here: it discards the request and returns a body the
 * Responses decoder rejects.
 */
type recordingHTTPClient struct{ userAgent string }

func (r *recordingHTTPClient) Do(req *http.Request) (*http.Response, error) {
	r.userAgent = req.Header.Get("User-Agent")
	body, err := json.Marshal(completedResponse("ok"))
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    req,
	}, nil
}

func TestNewOpenRouterSendsAgentUserAgentOnTheWire(t *testing.T) {
	transport := &recordingHTTPClient{}
	client := NewOpenRouter(OpenRouterOptions{SDK: SDKOptions{APIKey: "test", HTTPClient: transport}})

	result, err := client.CallModel(context.Background(), CallModelInput{Model: "openai/test", Input: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}

	tokens := strings.Fields(transport.userAgent)
	if len(tokens) == 0 {
		t.Fatalf("no User-Agent reached the transport")
	}
	last := tokens[len(tokens)-1]
	if last != agentModulePath && !strings.HasPrefix(last, agentModulePath+"/") {
		t.Fatalf("User-Agent = %q, want the agent token %q appended last", transport.userAgent, agentModulePath)
	}
	if !strings.Contains(transport.userAgent, "github.com/OpenRouterTeam/go-sdk") {
		t.Fatalf("User-Agent = %q, want the generated SDK token preserved", transport.userAgent)
	}
}
