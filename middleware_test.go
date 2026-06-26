package agent

import (
	"context"
	"io"
	"net/http"
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
