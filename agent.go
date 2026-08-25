// Package agent provides an idiomatic Go port of @openrouter/agent.
package agent

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
)

const agentModulePath = "github.com/OpenRouterTeam/go-agent"

// ResponseSender is the narrow Responses API surface used by CallModel.
type ResponseSender interface {
	SendResponse(ctx context.Context, request components.ResponsesRequest, metadata *components.MetadataLevel, opts ...operations.Option) (*operations.CreateResponsesResponse, error)
}

// RequestMiddleware observes or modifies an outgoing HTTP request.
type RequestMiddleware func(context.Context, *http.Request) error

// ResponseMiddleware observes a completed HTTP response.
type ResponseMiddleware func(context.Context, *http.Response) error

// ErrorMiddleware observes a transport or SDK HTTP error.
type ErrorMiddleware func(context.Context, *http.Request, error) error

// Hook is a shorthand request middleware, matching the upstream idea without exposing SDK internals.
type Hook = RequestMiddleware

// SDKOptions configures the underlying OpenRouter Go SDK client.
type SDKOptions struct {
	APIKey      string
	HTTPReferer string
	XTitle      string
	Timeout     time.Duration
	HTTPClient  openrouter.HTTPClient
}

// OpenRouterOptions configures the agent wrapper.
type OpenRouterOptions struct {
	SDK           SDKOptions
	BeforeRequest []RequestMiddleware
	AfterResponse []ResponseMiddleware
	AfterError    []ErrorMiddleware
	RequestHooks  []Hook
}

// OpenRouter wraps the official OpenRouter Go SDK and exposes agent-oriented helpers.
type OpenRouter struct {
	client *openrouter.OpenRouter
}

// NewOpenRouter constructs an agent wrapper. Middleware is installed through WithClient because
// the generated Go SDK keeps its hook package internal.
func NewOpenRouter(opts OpenRouterOptions) *OpenRouter {
	base := opts.SDK.HTTPClient
	if base == nil {
		base = http.DefaultClient
	}
	mwClient := &middlewareClient{
		base:       base,
		agentToken: agentUserAgentToken(),
		before:     append(append([]RequestMiddleware{}, opts.BeforeRequest...), opts.RequestHooks...),
		after:      append([]ResponseMiddleware{}, opts.AfterResponse...),
		afterErr:   append([]ErrorMiddleware{}, opts.AfterError...),
	}

	sdkOpts := []openrouter.SDKOption{openrouter.WithClient(mwClient)}
	if opts.SDK.APIKey != "" {
		sdkOpts = append(sdkOpts, openrouter.WithSecurity(opts.SDK.APIKey))
	}
	if opts.SDK.HTTPReferer != "" {
		sdkOpts = append(sdkOpts, openrouter.WithHTTPReferer(opts.SDK.HTTPReferer))
	}
	if opts.SDK.XTitle != "" {
		sdkOpts = append(sdkOpts, openrouter.WithXTitle(opts.SDK.XTitle))
	}
	if opts.SDK.Timeout > 0 {
		sdkOpts = append(sdkOpts, openrouter.WithTimeout(opts.SDK.Timeout))
	}
	return &OpenRouter{client: openrouter.New(sdkOpts...)}
}

// SDK returns the wrapped generated SDK client.
func (o *OpenRouter) SDK() *openrouter.OpenRouter { return o.client }

// SendResponse calls the generated Responses API endpoint.
func (o *OpenRouter) SendResponse(ctx context.Context, request components.ResponsesRequest, metadata *components.MetadataLevel, opts ...operations.Option) (*operations.CreateResponsesResponse, error) {
	return o.client.Beta.Responses.Send(ctx, request, metadata, opts...)
}

// CallModel invokes the package-level CallModel with this wrapper.
func (o *OpenRouter) CallModel(ctx context.Context, input CallModelInput) (*ModelResult, error) {
	return CallModel(ctx, o, input)
}

// SDKClient adapts a raw generated SDK client to ResponseSender.
type SDKClient struct{ Client *openrouter.OpenRouter }

// SendResponse calls Beta.Responses.Send on the wrapped generated SDK client.
func (s SDKClient) SendResponse(ctx context.Context, request components.ResponsesRequest, metadata *components.MetadataLevel, opts ...operations.Option) (*operations.CreateResponsesResponse, error) {
	return s.Client.Beta.Responses.Send(ctx, request, metadata, opts...)
}

type middlewareClient struct {
	base       openrouter.HTTPClient
	agentToken string
	before     []RequestMiddleware
	after      []ResponseMiddleware
	afterErr   []ErrorMiddleware
}

func (m *middlewareClient) Do(req *http.Request) (*http.Response, error) {
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	agentToken := m.agentToken
	if agentToken == "" {
		agentToken = agentUserAgentToken()
	}
	req.Header.Set("User-Agent", appendAgentUserAgent(req.Header.Get("User-Agent"), agentToken))
	ctx := req.Context()
	for _, hook := range m.before {
		if err := hook(ctx, req); err != nil {
			return nil, err
		}
	}
	res, err := m.base.Do(req)
	if err != nil {
		for _, hook := range m.afterErr {
			if hookErr := hook(ctx, req, err); hookErr != nil {
				return nil, hookErr
			}
		}
		return nil, err
	}
	for _, hook := range m.after {
		if err := hook(ctx, cloneResponseForHook(res)); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func agentUserAgentToken() string {
	buildInfo, ok := debug.ReadBuildInfo()
	if !ok {
		return agentModulePath
	}
	return agentUserAgentTokenFromBuildInfo(buildInfo)
}

func agentUserAgentTokenFromBuildInfo(buildInfo *debug.BuildInfo) string {
	if buildInfo == nil {
		return agentModulePath
	}
	modules := append([]*debug.Module{&buildInfo.Main}, buildInfo.Deps...)
	for _, module := range modules {
		if module == nil || module.Path != agentModulePath {
			continue
		}
		version := strings.TrimPrefix(module.Version, "v")
		if version == "" || version == "(devel)" {
			return agentModulePath
		}
		return agentModulePath + "/" + version
	}
	return agentModulePath
}

func appendAgentUserAgent(userAgent string, agentToken string) string {
	for _, token := range strings.Fields(userAgent) {
		if token == agentToken {
			return userAgent
		}
	}
	if userAgent == "" {
		return agentToken
	}
	return userAgent + " " + agentToken
}

func cloneResponseForHook(res *http.Response) *http.Response {
	clone := new(http.Response)
	*clone = *res
	if res.Body == nil {
		return clone
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		clone.Body = io.NopCloser(bytes.NewReader(nil))
		return clone
	}
	res.Body = io.NopCloser(bytes.NewReader(body))
	clone.Body = io.NopCloser(bytes.NewReader(body))
	return clone
}
