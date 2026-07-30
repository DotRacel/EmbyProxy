package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"embyproxy/internal/config"
	"embyproxy/internal/logging"
	"embyproxy/internal/storage"
)

func newFixesTestHandler(t *testing.T, node storage.Node, cfg config.Config) *Handler {
	t.Helper()
	store := newProxyTestStore(t)
	if err := store.SaveNode(context.Background(), "admin", node); err != nil {
		t.Fatal(err)
	}
	return New(cfg, store, nil, logging.New("silent", false))
}

func TestRequestBodyForReplayStreamsOversizedBody(t *testing.T) {
	h := &Handler{cfg: config.Config{Defaults: config.Defaults{MaxRetryBodyBytes: 8}}}
	payload := bytes.Repeat([]byte("x"), 64)
	req := httptest.NewRequest(http.MethodPost, "https://proxy.example/node/upload", bytes.NewReader(payload))

	body, streamed, err := h.requestBodyForReplay(req)
	if err != nil {
		t.Fatalf("requestBodyForReplay() error = %v", err)
	}
	if body != nil {
		t.Fatalf("body = %q, want nil for oversized body", body)
	}
	if streamed == nil {
		t.Fatal("streamed = nil, want streaming body")
	}
	reader, length, err := streamed.take()
	if err != nil {
		t.Fatalf("take() error = %v", err)
	}
	if length != int64(len(payload)) {
		t.Fatalf("length = %d, want %d", length, len(payload))
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("streamed %d bytes, want the full %d byte body", len(got), len(payload))
	}
	if _, _, err := streamed.take(); !errors.Is(err, errRequestBodyConsumed) {
		t.Fatalf("second take() error = %v, want errRequestBodyConsumed", err)
	}
}

func TestServeHTTPForwardsOversizedBodyWithoutTruncation(t *testing.T) {
	node := storage.Node{Name: "node", Secret: "secret", Target: "https://upstream.example"}
	h := newFixesTestHandler(t, node, config.Config{Defaults: config.Defaults{MaxRetryBodyBytes: 1024}})
	var upstreamBody []byte
	var upstreamLength int64
	h.noRedirectClient = noRedirectClient(func(req *http.Request) (*http.Response, error) {
		upstreamLength = req.ContentLength
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		upstreamBody = body
		return textResponse(http.StatusOK, "ok", nil), nil
	})
	payload := bytes.Repeat([]byte("a"), 4096)
	req := httptest.NewRequest(http.MethodPost, "https://proxy.example/node/secret/emby/Library/Refresh", bytes.NewReader(payload))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if len(upstreamBody) != len(payload) {
		t.Fatalf("upstream received %d bytes, want %d", len(upstreamBody), len(payload))
	}
	if !bytes.Equal(upstreamBody, payload) {
		t.Fatal("upstream body differs from client body")
	}
	if upstreamLength != int64(len(payload)) {
		t.Fatalf("upstream Content-Length = %d, want %d", upstreamLength, len(payload))
	}
}

func TestServeHTTPDoesNotFailOverOversizedBody(t *testing.T) {
	node := storage.Node{Name: "node", Secret: "secret", Target: "https://a.example,https://b.example"}
	h := newFixesTestHandler(t, node, config.Config{Defaults: config.Defaults{MaxRetryBodyBytes: 1024}})
	attempts := 0
	h.noRedirectClient = noRedirectClient(func(req *http.Request) (*http.Response, error) {
		attempts++
		_, _ = io.Copy(io.Discard, req.Body)
		return nil, errors.New("upstream down")
	})
	req := httptest.NewRequest(http.MethodPost, "https://proxy.example/node/secret/emby/Library/Refresh", bytes.NewReader(bytes.Repeat([]byte("a"), 4096)))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if attempts != 1 {
		t.Fatalf("upstream attempts = %d, want 1 (a streamed body cannot be replayed)", attempts)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
}

func TestParseRequestDecodesPathSegmentsOnce(t *testing.T) {
	h := &Handler{}
	tests := []struct {
		name   string
		target string
		want   string
	}{
		{name: "literal percent", target: "https://proxy.example/node/secret/media/100%25%20legit.mkv", want: "/media/100% legit.mkv"},
		{name: "encoded escape stays literal", target: "https://proxy.example/node/secret/media/a%2541b.mkv", want: "/media/a%41b.mkv"},
		{name: "encoded slash splits no extra segment", target: "https://proxy.example/node/secret/media/a%2Fb.mkv", want: "/media/a/b.mkv"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.target, nil)
			parsed, status, message := h.parseRequest(req)
			if status != 0 {
				t.Fatalf("parseRequest() status = %d (%s), want 0", status, message)
			}
			if got := buildRemainingPath(parsed.URL, parsed.Segments, 2); got != tt.want {
				t.Fatalf("remaining path = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestServeHTTPForwardsPercentInFilename(t *testing.T) {
	node := storage.Node{Name: "node", Secret: "secret", Target: "https://upstream.example"}
	h := newFixesTestHandler(t, node, config.Config{})
	gotPath := ""
	h.noRedirectClient = noRedirectClient(func(req *http.Request) (*http.Response, error) {
		gotPath = req.URL.EscapedPath()
		return textResponse(http.StatusOK, "ok", nil), nil
	})
	req := httptest.NewRequest(http.MethodGet, "https://proxy.example/node/secret/media/100%25%20legit.txt", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if gotPath != "/media/100%25%20legit.txt" {
		t.Fatalf("upstream path = %q, want %q", gotPath, "/media/100%25%20legit.txt")
	}
}

func TestHandleNodeReturns416WithoutBanningTarget(t *testing.T) {
	ctx := context.Background()
	node := storage.Node{Name: "node", Secret: "secret", Target: "https://a.example,https://b.example"}
	h := newFixesTestHandler(t, node, config.Config{})
	var hosts []string
	h.noRedirectClient = noRedirectClient(func(req *http.Request) (*http.Response, error) {
		hosts = append(hosts, req.URL.Host)
		return textResponse(http.StatusRequestedRangeNotSatisfiable, "", http.Header{"Content-Type": []string{"text/plain"}}), nil
	})
	parsed := parsedRoute{Name: "node", Secret: "secret", Path: "/media/movie.txt"}
	req := httptest.NewRequest(http.MethodGet, "https://proxy.example/node/secret/media/movie.txt", nil)
	req.Header.Set("Range", "bytes=999999999-")

	res, err := h.handleNode(ctx, req, node, parsed, nil, config.ProxyEnv{})
	if err != nil {
		t.Fatalf("handleNode() error = %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusRequestedRangeNotSatisfiable)
	}
	if len(hosts) != 1 {
		t.Fatalf("upstream attempts = %v, want a single attempt", hosts)
	}
	if _, banned := h.lineBan.Get("admin:node|https://a.example"); banned {
		t.Fatal("416 banned the target line")
	}

	res2, err := h.handleNode(ctx, req, node, parsed, nil, config.ProxyEnv{})
	if err != nil {
		t.Fatalf("handleNode() second call error = %v", err)
	}
	res2.Body.Close()
	if len(hosts) != 2 || hosts[1] != hosts[0] {
		t.Fatalf("upstream hosts = %v, want the same target twice", hosts)
	}
}

func TestSchemeHostHonorsForwardedProtoOnlyWhenTrusted(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://proxy.example/node/secret/emby", nil)
	req.Header.Set("X-Forwarded-Proto", "https")

	if got := schemeHost(req); got != "http://proxy.example" {
		t.Fatalf("schemeHost() = %q, want %q for an untrusted proxy header", got, "http://proxy.example")
	}

	trusted := req.WithContext(withTrustProxy(req.Context(), true))
	if got := schemeHost(trusted); got != "https://proxy.example" {
		t.Fatalf("schemeHost() = %q, want %q when the proxy is trusted", got, "https://proxy.example")
	}

	untrusted := req.WithContext(withTrustProxy(req.Context(), false))
	if got := schemeHost(untrusted); got != "http://proxy.example" {
		t.Fatalf("schemeHost() = %q, want %q when the proxy is not trusted", got, "http://proxy.example")
	}

	tlsReq := httptest.NewRequest(http.MethodGet, "http://proxy.example/node/secret/emby", nil)
	tlsReq.TLS = &tls.ConnectionState{}
	if got := schemeHost(tlsReq); got != "https://proxy.example" {
		t.Fatalf("schemeHost() = %q, want %q from the TLS fallback", got, "https://proxy.example")
	}
}

func TestServeHTTPIgnoresSpoofedForwardedProto(t *testing.T) {
	node := storage.Node{Name: "node", Secret: "secret", Target: "https://upstream.example"}
	h := newFixesTestHandler(t, node, config.Config{})
	origin := ""
	h.noRedirectClient = noRedirectClient(func(req *http.Request) (*http.Response, error) {
		origin = req.Header.Get("Origin")
		return textResponse(http.StatusOK, "ok", nil), nil
	})
	req := httptest.NewRequest(http.MethodGet, "http://proxy.example/node/secret/emby/Users/AuthenticateByName", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.HasPrefix(origin, "http://") {
		t.Fatalf("outbound Origin = %q, want the spoofed X-Forwarded-Proto to be ignored", origin)
	}
}
