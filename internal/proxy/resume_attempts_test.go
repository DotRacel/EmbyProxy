package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"embyproxy/internal/logging"
)

// chunkThenFailBody hands out one fixed-size chunk and then reports a network
// error, modelling an upstream that keeps dropping the connection mid-transfer.
type chunkThenFailBody struct {
	remaining int
	chunk     int
	err       error
}

func (b *chunkThenFailBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, b.err
	}
	n := b.chunk
	if n > len(p) {
		n = len(p)
	}
	if n > b.remaining {
		n = b.remaining
	}
	for i := range p[:n] {
		p[i] = 'x'
	}
	b.remaining -= n
	return n, nil
}

func (b *chunkThenFailBody) Close() error { return nil }

// resumeAttemptFixture drives a full body copy where the upstream delivers
// chunkBytes on the initial response and on every resume before failing again.
type resumeAttemptFixture struct {
	total      int64
	chunkBytes int
	resumes    int
}

func (f *resumeAttemptFixture) run(t *testing.T) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	upstreamErr := errors.New("connection reset by peer")

	header := func(start int64) http.Header {
		h := http.Header{}
		h.Set("Accept-Ranges", "bytes")
		h.Set("Content-Type", "video/x-matroska")
		h.Set("ETag", `"media-v1"`)
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, f.total-1, f.total))
		h.Set("Content-Length", fmt.Sprintf("%d", f.total-start))
		return h
	}

	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		f.resumes++
		var start int64
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-", &start); err != nil {
			return nil, err
		}
		out := &http.Response{
			StatusCode:    http.StatusPartialContent,
			Status:        "206 Partial Content",
			ContentLength: f.total - start,
			Header:        header(start),
			Body:          &chunkThenFailBody{remaining: f.chunkBytes, chunk: f.chunkBytes, err: upstreamErr},
			Request:       req,
		}
		return out, nil
	})}

	req := httptest.NewRequest(http.MethodGet, "/node/secret/emby/videos/1/original.mkv", nil)
	req.Header.Set("Range", "bytes=0-")
	req = req.WithContext(WithAccessLogFields(req.Context()))

	upstreamReq := httptest.NewRequest(http.MethodGet, "https://cdn.example/video/original.mkv", nil)
	upstreamReq.Header.Set("Range", "bytes=0-")

	res := &http.Response{
		StatusCode:    http.StatusPartialContent,
		Status:        "206 Partial Content",
		ContentLength: f.total,
		Header:        header(0),
		Body:          &chunkThenFailBody{remaining: f.chunkBytes, chunk: f.chunkBytes, err: upstreamErr},
		Request:       upstreamReq,
	}
	attachUpstreamClient(res, client)
	markStreamResumeCandidate(res, "direct")

	rec := httptest.NewRecorder()
	(&Handler{log: logging.New("silent", false)}).sendResponse(rec, req, res)
	return rec, req
}

// A long transfer that keeps making progress must not be aborted just because it
// needed more than streamResumeMaxAttempts resumes in total.
func TestStreamResumeSurvivesManyProgressingRetries(t *testing.T) {
	const total = 1 << 20
	const chunk = 128 * 1024
	f := &resumeAttemptFixture{total: total, chunkBytes: chunk}

	rec, req := f.run(t)

	if got := int64(rec.Body.Len()); got != total {
		t.Fatalf("delivered %d bytes, want %d", got, total)
	}
	wantResumes := total/chunk - 1
	if f.resumes != wantResumes {
		t.Fatalf("resume requests = %d, want %d", f.resumes, wantResumes)
	}
	if f.resumes <= streamResumeMaxAttempts {
		t.Fatalf("fixture needs more than %d resumes to be meaningful, got %d", streamResumeMaxAttempts, f.resumes)
	}
	if fields := AccessLogFields(req.Context()); fields["streamResumeError"] != nil {
		t.Fatalf("streamResumeError = %v, want none", fields["streamResumeError"])
	}
}

// An upstream that reconnects but never delivers anything must still be given up
// on, otherwise the copy loop would spin forever.
func TestStreamResumeGivesUpWithoutProgress(t *testing.T) {
	f := &resumeAttemptFixture{total: 1 << 20, chunkBytes: 0}

	_, req := f.run(t)

	if f.resumes != streamResumeMaxAttempts {
		t.Fatalf("resume requests = %d, want %d", f.resumes, streamResumeMaxAttempts)
	}
	fields := AccessLogFields(req.Context())
	msg, _ := fields["streamResumeError"].(string)
	if !strings.Contains(msg, "attempts exhausted") {
		t.Fatalf("streamResumeError = %q, want it to report exhausted attempts", msg)
	}
}

// Progress below the threshold must not refresh the budget, or a trickling
// upstream would keep the loop alive indefinitely.
func TestStreamResumeTreatsTrickleAsNoProgress(t *testing.T) {
	f := &resumeAttemptFixture{total: 1 << 20, chunkBytes: 1024}

	_, req := f.run(t)

	if f.resumes > streamResumeMaxTotalAttempts {
		t.Fatalf("resume requests = %d, want at most %d", f.resumes, streamResumeMaxTotalAttempts)
	}
	if f.resumes != streamResumeMaxAttempts {
		t.Fatalf("resume requests = %d, want %d", f.resumes, streamResumeMaxAttempts)
	}
	fields := AccessLogFields(req.Context())
	msg, _ := fields["streamResumeError"].(string)
	if !strings.Contains(msg, "attempts exhausted") {
		t.Fatalf("streamResumeError = %q, want it to report exhausted attempts", msg)
	}
}
