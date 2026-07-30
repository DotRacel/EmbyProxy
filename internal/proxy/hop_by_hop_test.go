package proxy

import (
	"net/http"
	"net/url"
	"testing"

	"embyproxy/internal/config"
	"embyproxy/internal/identity"
	"embyproxy/internal/storage"
)

func TestStripProxyMetadataHeadersRemovesHopByHopHeaders(t *testing.T) {
	tests := []struct {
		name       string
		raw        http.Header
		wantAbsent []string
		wantKeep   map[string]string
	}{
		{
			name: "connection close",
			raw: http.Header{
				"Connection":    {"close"},
				"Range":         {"bytes=0-1023"},
				"Authorization": {`Emby Token="abc"`},
			},
			wantAbsent: []string{"Connection"},
			wantKeep: map[string]string{
				"Range":         "bytes=0-1023",
				"Authorization": `Emby Token="abc"`,
			},
		},
		{
			name: "connection listed token",
			raw: http.Header{
				"Connection":    {"x-custom-hop"},
				"X-Custom-Hop":  {"drop-me"},
				"Range":         {"bytes=0-"},
				"Authorization": {"Bearer token"},
			},
			wantAbsent: []string{"Connection", "X-Custom-Hop"},
			wantKeep: map[string]string{
				"Range":         "bytes=0-",
				"Authorization": "Bearer token",
			},
		},
		{
			name: "connection multi token and fixed set",
			raw: http.Header{
				"Connection":          {"keep-alive, X-Custom-Hop"},
				"Keep-Alive":          {"timeout=5"},
				"X-Custom-Hop":        {"drop-me"},
				"Proxy-Connection":    {"keep-alive"},
				"Proxy-Authenticate":  {"Basic"},
				"Proxy-Authorization": {"Basic Zm9v"},
				"Te":                  {"trailers"},
				"Trailer":             {"Expires"},
				"Transfer-Encoding":   {"chunked"},
				"Upgrade":             {"h2c"},
				"Accept":              {"application/json"},
			},
			wantAbsent: []string{
				"Connection", "Keep-Alive", "X-Custom-Hop", "Proxy-Connection",
				"Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer",
				"Transfer-Encoding", "Upgrade",
			},
			wantKeep: map[string]string{"Accept": "application/json"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := cloneHeader(tt.raw)
			stripProxyMetadataHeaders(h)
			assertHeaderKeysAbsent(t, h, tt.wantAbsent...)
			for key, want := range tt.wantKeep {
				if got := h.Get(key); got != want {
					t.Fatalf("%s = %q, want %q", key, got, want)
				}
			}
		})
	}
}

func TestOutboundHeaderBuildersRemoveHopByHopHeaders(t *testing.T) {
	targetURL, err := url.Parse("https://upstream.example/emby/Items")
	if err != nil {
		t.Fatal(err)
	}
	ids := identity.NewManager(nil)
	node := storage.Node{}
	raw := http.Header{
		"Connection":   {"close, X-Custom-Hop"},
		"X-Custom-Hop": {"drop-me"},
		"Keep-Alive":   {"timeout=5"},
		"Range":        {"bytes=0-"},
		"User-Agent":   {"Original/1.0"},
	}
	tests := []struct {
		name  string
		build func(http.Header) http.Header
	}{
		{
			name: "clean proxy",
			build: func(raw http.Header) http.Header {
				return buildCleanProxyHeaders(ids, raw, targetURL, node, config.ProxyEnv{}, true)
			},
		},
		{
			name: "direct",
			build: func(raw http.Header) http.Header {
				return buildDirectOutboundHeaders(ids, raw, targetURL, config.ProxyEnv{}, node, "normal")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := tt.build(raw)
			assertHeaderKeysAbsent(t, headers, "Connection", "X-Custom-Hop", "Keep-Alive")
			if got := headers.Get("Range"); got != "bytes=0-" {
				t.Fatalf("Range = %q, want bytes=0-", got)
			}
			if got := headers.Get("User-Agent"); got != "Original/1.0" {
				t.Fatalf("User-Agent = %q, want Original/1.0", got)
			}
		})
	}
}

func TestBuildWebSocketHeadersKeepsUpgradeAfterHopByHopStrip(t *testing.T) {
	targetURL, err := url.Parse("wss://upstream.example/emby/Sessions/1/WebSocket")
	if err != nil {
		t.Fatal(err)
	}
	ids := identity.NewManager(nil)
	node := storage.Node{}
	raw := http.Header{
		"Connection":            {"Upgrade, X-Custom-Hop"},
		"X-Custom-Hop":          {"drop-me"},
		"Upgrade":               {"websocket"},
		"Keep-Alive":            {"timeout=5"},
		"Sec-Websocket-Key":     {"dGhlIHNhbXBsZSBub25jZQ=="},
		"Sec-Websocket-Version": {"13"},
		"User-Agent":            {"Original/1.0"},
	}

	headers := buildWebSocketHeaders(ids, raw, targetURL, node)

	if got := headers.Get("Connection"); got != "Upgrade" {
		t.Fatalf("Connection = %q, want Upgrade", got)
	}
	if got := headers.Get("Upgrade"); got != "websocket" {
		t.Fatalf("Upgrade = %q, want websocket", got)
	}
	assertHeaderKeysAbsent(t, headers, "X-Custom-Hop", "Keep-Alive")
	if got := headers.Get("Sec-WebSocket-Key"); got != "dGhlIHNhbXBsZSBub25jZQ==" {
		t.Fatalf("Sec-WebSocket-Key = %q, want passthrough", got)
	}
	if got := headers.Get("Sec-WebSocket-Version"); got != "13" {
		t.Fatalf("Sec-WebSocket-Version = %q, want 13", got)
	}
}

func TestCopyResponseHeadersDropsHopByHopHeaders(t *testing.T) {
	src := http.Header{
		"Connection":         {"close"},
		"Keep-Alive":         {"timeout=5"},
		"Upgrade":            {"h2c"},
		"Proxy-Authenticate": {"Basic"},
		"Transfer-Encoding":  {"chunked"},
		"Content-Type":       {"video/mp4"},
		"Content-Length":     {"1024"},
		"Accept-Ranges":      {"bytes"},
		"Etag":               {`"abc"`},
	}
	dst := http.Header{}

	copyResponseHeaders(dst, src, false)

	assertHeaderKeysAbsent(t, dst,
		"Connection", "Keep-Alive", "Upgrade", "Proxy-Authenticate", "Transfer-Encoding",
	)
	for key, want := range map[string]string{
		"Content-Type":   "video/mp4",
		"Content-Length": "1024",
		"Accept-Ranges":  "bytes",
		"Etag":           `"abc"`,
	} {
		if got := dst.Get(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestCopyResponseHeadersDropsConnectionListedTokens(t *testing.T) {
	src := http.Header{
		"Connection":     {"close, X-Upstream-Hop"},
		"X-Upstream-Hop": {"drop-me"},
		"Content-Type":   {"application/json"},
	}
	dst := http.Header{}

	copyResponseHeaders(dst, src, false)

	assertHeaderKeysAbsent(t, dst, "Connection", "X-Upstream-Hop")
	if got := dst.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
}

func TestCopyResponseHeadersKeepsDecodedBodySkipWithHopByHopRemoval(t *testing.T) {
	src := http.Header{
		"Connection":       {"close"},
		"Content-Encoding": {"gzip"},
		"Content-Length":   {"512"},
		"Content-Md5":      {"deadbeef"},
		"Content-Type":     {"application/json"},
	}
	dst := http.Header{}

	copyResponseHeaders(dst, src, true)

	assertHeaderKeysAbsent(t, dst, "Connection", "Content-Encoding", "Content-Length", "Content-Md5")
	if got := dst.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
}
