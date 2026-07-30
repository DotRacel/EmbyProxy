package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"embyproxy/internal/config"
	"embyproxy/internal/logging"
	"embyproxy/internal/storage"
)

func TestApplyBodyLinkReplacementsRewritesLongestURLFirst(t *testing.T) {
	short := "http://cdn.example/x.m3u8"
	long := short + "?token=abc"
	replacements := map[string]string{
		short: "https://proxy.example/node/secret/x.m3u8",
		long:  "https://proxy.example/node/secret/x.m3u8?token=abc",
	}
	text := "#EXTM3U\n" + short + "\n" + long + "\n"
	want := "#EXTM3U\n" + replacements[short] + "\n" + replacements[long] + "\n"

	for i := 0; i < 32; i++ {
		if got := applyBodyLinkReplacements(text, replacements); got != want {
			t.Fatalf("applyBodyLinkReplacements() = %q, want %q", got, want)
		}
	}
}

func TestApplyBodyLinkReplacementsIsDeterministicForSameLengthKeys(t *testing.T) {
	replacements := map[string]string{
		"http://a.example/1.ts": "https://proxy.example/a/1.ts",
		"http://b.example/1.ts": "https://proxy.example/b/1.ts",
	}
	text := "http://a.example/1.ts http://b.example/1.ts"
	want := "https://proxy.example/a/1.ts https://proxy.example/b/1.ts"

	for i := 0; i < 32; i++ {
		if got := applyBodyLinkReplacements(text, replacements); got != want {
			t.Fatalf("applyBodyLinkReplacements() = %q, want %q", got, want)
		}
	}
}

func TestRewriteBodyLinksKeepsPrefixCollidingURLsIntact(t *testing.T) {
	ctx := context.Background()
	store := newProxyTestStore(t)
	node := storage.Node{Name: "node", Secret: "secret", Target: "https://upstream.example"}
	h := New(config.Config{AdminToken: "test-admin-token"}, store, nil, logging.New("silent", false))
	short := "https://cdn.example/x.m3u8"
	long := short + "?token=abc"

	out := h.rewriteBodyLinks(ctx, "#EXTM3U\n"+short+"\n"+long+"\n", "https://proxy.example/node/secret/playlist.m3u8", node, "node", "secret")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("rewritten manifest lines = %q", lines)
	}
	assertSignedRawLink(t, h, lines[1], "https://proxy.example", "/node/secret", short)
	assertSignedRawLink(t, h, lines[2], "https://proxy.example", "/node/secret", long)
}

func TestShouldRetryWithoutRange(t *testing.T) {
	tests := []struct {
		name        string
		rangeHeader string
		status      int
		headers     http.Header
		want        bool
	}{
		{name: "200 without content-range", rangeHeader: "bytes=0-", status: http.StatusOK, want: false},
		{name: "206 partial content", rangeHeader: "bytes=0-", status: http.StatusPartialContent, headers: http.Header{"Content-Range": []string{"bytes 0-4/5"}}, want: false},
		{name: "302 without content-range", rangeHeader: "bytes=0-", status: http.StatusFound, want: false},
		{name: "416 range not satisfiable", rangeHeader: "bytes=0-", status: http.StatusRequestedRangeNotSatisfiable, want: true},
		{name: "500 without content-range", rangeHeader: "bytes=0-", status: http.StatusInternalServerError, want: true},
		{name: "500 with content-range", rangeHeader: "bytes=0-", status: http.StatusInternalServerError, headers: http.Header{"Content-Range": []string{"bytes 0-4/5"}}, want: false},
		{name: "no range header", rangeHeader: "", status: http.StatusInternalServerError, want: false},
		{name: "range not starting at zero", rangeHeader: "bytes=1024-", status: http.StatusInternalServerError, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := tt.headers
			if headers == nil {
				headers = http.Header{}
			}
			res := &http.Response{StatusCode: tt.status, Header: headers}
			if got := shouldRetryWithoutRange(tt.rangeHeader, res); got != tt.want {
				t.Fatalf("shouldRetryWithoutRange(%q, %d) = %v, want %v", tt.rangeHeader, tt.status, got, tt.want)
			}
		})
	}
}

func TestHandleDirectDoesNotRefetchWhenUpstreamIgnoresRange(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		wantCalls  int
		wantRanges []string
	}{
		{name: "200 without content-range", status: http.StatusOK, wantCalls: 1, wantRanges: []string{"bytes=0-"}},
		{name: "416 range not satisfiable", status: http.StatusRequestedRangeNotSatisfiable, wantCalls: 2, wantRanges: []string{"bytes=0-", ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ranges := []string{}
			h, calls := newRawServeHTTPHarness(t, config.Config{AdminToken: "test-admin-token"}, func(req *http.Request) (*http.Response, error) {
				ranges = append(ranges, req.Header.Get("Range"))
				if len(ranges) == 1 {
					return textResponse(tt.status, "full-body", nil), nil
				}
				return textResponse(http.StatusOK, "full-body", nil), nil
			})
			req := httptest.NewRequest(http.MethodGet, h.signedRawLink("https://proxy.example", "/node/secret", "http://8.8.8.8/video.mkv"), nil)
			req.Header.Set("Range", "bytes=0-")
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body = %q", rec.Code, http.StatusOK, rec.Body.String())
			}
			if *calls != tt.wantCalls {
				t.Fatalf("upstream calls = %d, want %d (ranges=%v)", *calls, tt.wantCalls, ranges)
			}
			if len(ranges) != len(tt.wantRanges) {
				t.Fatalf("upstream ranges = %v, want %v", ranges, tt.wantRanges)
			}
			for i, want := range tt.wantRanges {
				if ranges[i] != want {
					t.Fatalf("upstream request %d Range = %q, want %q", i, ranges[i], want)
				}
			}
		})
	}
}

func TestShouldRetryWithoutRangeNilResponse(t *testing.T) {
	if shouldRetryWithoutRange("bytes=0-", nil) {
		t.Fatal("shouldRetryWithoutRange() with nil response = true, want false")
	}
}
