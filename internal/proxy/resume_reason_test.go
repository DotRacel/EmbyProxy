package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"embyproxy/internal/logging"
)

// newResumeReasonResponse builds a 206 media response that satisfies every
// streamResumePlan gate, so each subtest only has to break the one it exercises.
func newResumeReasonResponse() (*http.Request, *http.Response) {
	req := httptest.NewRequest(http.MethodGet, "/node/secret/emby/videos/1/original.mkv", nil)
	req.Header.Set("Range", "bytes=142-")

	upstreamReq := httptest.NewRequest(http.MethodGet, "https://cdn.example/video/original.mkv", nil)
	upstreamReq.Header.Set("Range", "bytes=142-")

	// Build the header with Set so every key is canonical: a literal "ETag" key
	// is not canonical, and a later Del("ETag") would silently miss it.
	header := http.Header{}
	header.Set("Accept-Ranges", "bytes")
	header.Set("Content-Length", "11")
	header.Set("Content-Range", "bytes 142-152/153")
	header.Set("Content-Type", "video/x-matroska")
	header.Set("ETag", `"media-v1"`)

	res := &http.Response{
		StatusCode:    http.StatusPartialContent,
		Status:        "206 Partial Content",
		ContentLength: 11,
		Header:        header,
		Body:          io.NopCloser(strings.NewReader("hello world")),
		Request:       upstreamReq,
	}
	attachUpstreamClient(res, &http.Client{})
	markStreamResumeCandidate(res, "direct")
	return req, res
}

func TestStreamResumePlanWithReasonNamesTheRejectingGate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*http.Request, *http.Response)
		wantOK  bool
		wantWhy string
	}{
		{name: "healthy media response resumes", mutate: func(*http.Request, *http.Response) {}, wantOK: true},
		{
			name:    "missing validator",
			mutate:  func(_ *http.Request, res *http.Response) { res.Header.Del("ETag") },
			wantWhy: "no-validator",
		},
		{
			name: "weak etag",
			mutate: func(_ *http.Request, res *http.Response) {
				res.Header.Set("ETag", `W/"media-v1"`)
			},
			wantWhy: "weak-etag",
		},
		{
			name: "last modified without usable date",
			mutate: func(_ *http.Request, res *http.Response) {
				res.Header.Del("ETag")
				res.Header.Set("Last-Modified", "Wed, 30 Jul 2026 13:00:00 GMT")
			},
			wantWhy: "unusable-last-modified",
		},
		{
			name: "last modified with date resumes",
			mutate: func(_ *http.Request, res *http.Response) {
				res.Header.Del("ETag")
				res.Header.Set("Last-Modified", "Wed, 30 Jul 2026 13:00:00 GMT")
				res.Header.Set("Date", "Wed, 30 Jul 2026 13:00:05 GMT")
			},
			wantOK: true,
		},
		{
			name:    "upstream refuses ranges",
			mutate:  func(_ *http.Request, res *http.Response) { res.Header.Del("Accept-Ranges") },
			wantWhy: "no-accept-ranges",
		},
		{
			name: "not a media response",
			mutate: func(_ *http.Request, res *http.Response) {
				res.Request.URL.Path = "/api/items"
				res.Header.Set("Content-Type", "application/json")
			},
			wantWhy: "not-media",
		},
		{
			name: "compressed body cannot be resumed",
			mutate: func(_ *http.Request, res *http.Response) {
				res.Header.Set("Content-Encoding", "gzip")
			},
			wantWhy: "content-encoded",
		},
		{
			name: "response was never marked streaming",
			mutate: func(_ *http.Request, res *http.Response) {
				// The marker lives in the upstream request context, so it has to be
				// dropped rather than cloned.
				res.Request = res.Request.Clone(context.Background())
			},
			wantWhy: "not-stream-candidate",
		},
		{
			name: "multi range request",
			mutate: func(_ *http.Request, res *http.Response) {
				res.Request.Header.Set("Range", "bytes=0-10,20-30")
			},
			wantWhy: "not-single-byte-range",
		},
		{
			name: "unparsable content range",
			mutate: func(_ *http.Request, res *http.Response) {
				res.Header.Set("Content-Range", "bytes 142-152/*")
			},
			wantWhy: "bad-content-range",
		},
		{
			name:    "non GET",
			mutate:  func(r *http.Request, _ *http.Response) { r.Method = http.MethodHead },
			wantWhy: "not-get",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, res := newResumeReasonResponse()
			tc.mutate(req, res)
			_, ok, why := (&Handler{}).streamResumePlanWithReason(req, res)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (reason %q)", ok, tc.wantOK, why)
			}
			if ok {
				if why != "" {
					t.Fatalf("reason = %q, want empty on success", why)
				}
				return
			}
			if why != tc.wantWhy {
				t.Fatalf("reason = %q, want %q", why, tc.wantWhy)
			}
		})
	}
}

func TestSendResponseLogsWhyResumeIsDisabled(t *testing.T) {
	req, res := newResumeReasonResponse()
	res.Header.Del("ETag")

	log := logging.New("debug", false)
	(&Handler{log: log}).sendResponse(httptest.NewRecorder(), req, res)

	var line string
	for _, entry := range log.Entries(50) {
		if strings.Contains(entry.Line, "event=streamResumeDisabled") {
			line = entry.Line
			break
		}
	}
	if line == "" {
		t.Fatal("no streamResumeDisabled entry was logged")
	}
	for _, want := range []string{"reason=no-validator", "contentType=video/x-matroska", "acceptRanges=bytes"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line %q missing %q", line, want)
		}
	}
}
