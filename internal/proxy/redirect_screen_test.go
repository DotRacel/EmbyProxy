package proxy

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func newRedirectHop(t *testing.T, ctx context.Context, rawURL string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// The upstream image endpoint redirects onto its own host (/emby/Items/../Images
// -> /img/..). That origin was configured by the admin and already dialed, so
// the hop must not depend on a fresh DNS lookup succeeding.
func TestScreenFollowRedirectAllowsSameOriginWithoutLookup(t *testing.T) {
	first := newRedirectHop(t, context.Background(), "https://media.invalid/emby/Items/1/Images/Primary")
	hop := newRedirectHop(t, context.Background(), "https://MEDIA.invalid:443/img/i/poster/1.jpg")

	if err := screenFollowRedirect(hop, []*http.Request{first}); err != nil {
		t.Fatalf("screenFollowRedirect() = %v, want nil for a same-origin hop", err)
	}
}

func TestScreenFollowRedirectScreensSameHostOnOtherPort(t *testing.T) {
	first := newRedirectHop(t, context.Background(), "http://127.0.0.1:8096/emby/Videos/1/stream")
	hop := newRedirectHop(t, context.Background(), "http://127.0.0.1:2375/containers/json")

	err := screenFollowRedirect(hop, []*http.Request{first})
	if err == nil || !strings.Contains(err.Error(), "blocked redirect to internal host") {
		t.Fatalf("screenFollowRedirect() = %v, want internal host block", err)
	}
}

func TestScreenFollowRedirectReportsClientCancelInsteadOfBlock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	first := newRedirectHop(t, ctx, "https://upstream.example/emby/Videos/1/stream")
	hop := newRedirectHop(t, ctx, "https://cdn.example/video.mkv")

	err := screenFollowRedirect(hop, []*http.Request{first})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("screenFollowRedirect() = %v, want context.Canceled", err)
	}
	if strings.Contains(err.Error(), "internal host") {
		t.Fatalf("screenFollowRedirect() = %q, a canceled lookup must not read as an internal-host block", err)
	}
}

func TestScreenFollowRedirectFailsClosedOnLookupError(t *testing.T) {
	first := newRedirectHop(t, context.Background(), "https://upstream.example/emby/Videos/1/stream")
	hop := newRedirectHop(t, context.Background(), "https://cdn.invalid/video.mkv")

	err := screenFollowRedirect(hop, []*http.Request{first})
	if err == nil {
		t.Fatal("screenFollowRedirect() = nil, want unresolvable cross-origin hop rejected")
	}
	if !strings.Contains(err.Error(), "redirect host lookup failed") {
		t.Fatalf("screenFollowRedirect() = %q, want a lookup failure rather than an internal-host block", err)
	}
}

func TestScreenFollowRedirectBlocksPrivateAddress(t *testing.T) {
	first := newRedirectHop(t, context.Background(), "https://upstream.example/emby/Videos/1/stream")
	hop := newRedirectHop(t, context.Background(), "http://169.254.169.254/latest/meta-data/")

	err := screenFollowRedirect(hop, []*http.Request{first})
	if err == nil || !strings.Contains(err.Error(), "blocked redirect to internal host: 169.254.169.254") {
		t.Fatalf("screenFollowRedirect() = %v, want internal host block", err)
	}
}
