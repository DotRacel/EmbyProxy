package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"embyproxy/internal/config"
	"embyproxy/internal/storage"
)

const (
	lineBanTestA = "https://a.example"
	lineBanTestB = "https://b.example"
)

func newLineBanTestHandler(t *testing.T, statusFor func(host string) (int, error)) (*Handler, storage.Node, *[]string) {
	t.Helper()
	node := storage.Node{Name: "node", Secret: "secret", Target: lineBanTestA + "," + lineBanTestB}
	h := newFixesTestHandler(t, node, config.Config{})
	var hosts []string
	h.noRedirectClient = noRedirectClient(func(req *http.Request) (*http.Response, error) {
		hosts = append(hosts, req.URL.Host)
		status, err := statusFor(req.URL.Host)
		if err != nil {
			return nil, err
		}
		return textResponse(status, "", http.Header{"Content-Type": []string{"text/plain"}}), nil
	})
	return h, node, &hosts
}

func lineBanTestRequest(t *testing.T, h *Handler, ctx context.Context, node storage.Node) {
	t.Helper()
	parsed := parsedRoute{Name: "node", Secret: "secret", Path: "/emby/System/Info"}
	req := httptest.NewRequest(http.MethodGet, "https://proxy.example/node/secret/emby/System/Info", nil).WithContext(ctx)
	res, err := h.handleNode(ctx, req, node, parsed, nil, config.ProxyEnv{})
	if err != nil && ctx.Err() == nil {
		t.Fatalf("handleNode() error = %v", err)
	}
	if res != nil {
		res.Body.Close()
	}
}

func lineBanned(h *Handler, target string) bool {
	_, banned := h.lineBan.Get(lineBanKey("admin:node", target))
	return banned
}

// A 403/404 is about the requested path (scanners probing /.env produce
// thousands of them), not about the line, so it may fail over but never bench
// the line for every other viewer.
func TestHandleNodeFailsOverOnNotFoundWithoutBanningLine(t *testing.T) {
	h, node, hosts := newLineBanTestHandler(t, func(host string) (int, error) {
		if host == "a.example" {
			return http.StatusNotFound, nil
		}
		return http.StatusOK, nil
	})

	for i := 0; i < lineBanStrikes+2; i++ {
		lineBanTestRequest(t, h, context.Background(), node)
	}

	if (*hosts)[0] != "a.example" || (*hosts)[1] != "b.example" {
		t.Fatalf("upstream hosts = %v, want a 404 on a to fail over to b", *hosts)
	}
	if lineBanned(h, lineBanTestA) {
		t.Fatal("404 banned the target line")
	}
}

func TestHandleNodeDoesNotBanLineWhenClientCancels(t *testing.T) {
	h, node, _ := newLineBanTestHandler(t, func(string) (int, error) {
		return 0, context.Canceled
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for i := 0; i < lineBanStrikes+2; i++ {
		lineBanTestRequest(t, h, ctx, node)
	}

	if lineBanned(h, lineBanTestA) || lineBanned(h, lineBanTestB) {
		t.Fatal("client cancellation banned a target line")
	}
}

func TestHandleNodeBansLineOnlyAfterRepeatedFailures(t *testing.T) {
	h, node, _ := newLineBanTestHandler(t, func(host string) (int, error) {
		if host == "a.example" {
			return 0, errors.New("connection reset by peer")
		}
		return http.StatusBadGateway, nil
	})

	for i := 0; i < lineBanStrikes-1; i++ {
		lineBanTestRequest(t, h, context.Background(), node)
		if lineBanned(h, lineBanTestA) || lineBanned(h, lineBanTestB) {
			t.Fatalf("line banned after %d failures, want %d", i+1, lineBanStrikes)
		}
	}
	lineBanTestRequest(t, h, context.Background(), node)
	if !lineBanned(h, lineBanTestA) {
		t.Fatal("transport errors did not ban line a after repeated failures")
	}
	if !lineBanned(h, lineBanTestB) {
		t.Fatal("5xx answers did not ban line b after repeated failures")
	}
}

func TestHandleNodeSuccessResetsLineStrikes(t *testing.T) {
	failing := true
	h, node, _ := newLineBanTestHandler(t, func(host string) (int, error) {
		if failing {
			return http.StatusServiceUnavailable, nil
		}
		return http.StatusOK, nil
	})

	for i := 0; i < lineBanStrikes-1; i++ {
		lineBanTestRequest(t, h, context.Background(), node)
	}
	failing = false
	lineBanTestRequest(t, h, context.Background(), node)
	failing = true
	for i := 0; i < lineBanStrikes-1; i++ {
		lineBanTestRequest(t, h, context.Background(), node)
	}

	if lineBanned(h, lineBanTestA) {
		t.Fatal("line banned although a success separated the failures")
	}
}

func TestHandleNodeSuccessLiftsBan(t *testing.T) {
	h, node, _ := newLineBanTestHandler(t, func(string) (int, error) {
		return http.StatusOK, nil
	})
	h.lineBan.Set(lineBanKey("admin:node", lineBanTestA), 1, lineBanDuration)
	h.lineBan.Set(lineBanKey("admin:node", lineBanTestB), 1, lineBanDuration)

	lineBanTestRequest(t, h, context.Background(), node)

	if lineBanned(h, lineBanTestA) {
		t.Fatal("a line that just answered successfully is still banned")
	}
}

func TestWebSocketUpstreamRejectionDoesNotBanLine(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer upstream.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer second.Close()
	node := storage.Node{Name: "node", Secret: "secret", Target: upstream.URL + "," + second.URL}
	h := newFixesTestHandler(t, node, config.Config{})
	parsed := parsedRoute{Name: "node", Secret: "secret", Path: "/"}

	for i := 0; i < lineBanStrikes+1; i++ {
		req := httptest.NewRequest(http.MethodGet, "https://proxy.example/node/secret/", nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		rec := httptest.NewRecorder()
		h.handleWebSocket(rec, req, node, parsed)
	}

	if lineBanned(h, upstream.URL) || lineBanned(h, second.URL) {
		t.Fatal("an upstream 404 to a websocket upgrade banned the line")
	}
}
