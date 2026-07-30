package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	// failResumes makes the first N resume requests fail at the transport level
	// (-1 fails every one of them), modelling a blip that hits the resume
	// request itself rather than the stream.
	failResumes int
	// mutateResume rewrites the headers of every resume response, so a test can
	// hand back an answer the validator must reject.
	mutateResume func(http.Header)
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
		if f.failResumes < 0 || f.resumes <= f.failResumes {
			return nil, errors.New("dial tcp cdn.example:443: connect: connection refused")
		}
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
		if f.mutateResume != nil {
			f.mutateResume(out.Header)
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

// shrinkStreamResumeBackoff keeps the retry pause out of the test runtime.
func shrinkStreamResumeBackoff(t *testing.T) {
	t.Helper()
	previous := streamResumeRetryBackoff
	streamResumeRetryBackoff = time.Millisecond
	t.Cleanup(func() { streamResumeRetryBackoff = previous })
}

// A transient failure of the resume request itself must not kill a stream that
// still has budget left: the retry simply costs another attempt.
func TestStreamResumeRetriesTransientResumeRequestFailure(t *testing.T) {
	shrinkStreamResumeBackoff(t)
	const total = 1 << 20
	const chunk = 128 * 1024
	f := &resumeAttemptFixture{total: total, chunkBytes: chunk, failResumes: 1}

	rec, req := f.run(t)

	if got := int64(rec.Body.Len()); got != total {
		t.Fatalf("delivered %d bytes, want %d", got, total)
	}
	// One resume per missing chunk, plus the request that failed to connect.
	wantResumes := total/chunk - 1 + 1
	if f.resumes != wantResumes {
		t.Fatalf("resume requests = %d, want %d", f.resumes, wantResumes)
	}
	fields := AccessLogFields(req.Context())
	if got := fields["streamResumeAttempts"]; got != wantResumes {
		t.Fatalf("streamResumeAttempts = %v, want %d", got, wantResumes)
	}
	if fields["streamResumeError"] != nil {
		t.Fatalf("streamResumeError = %v, want none", fields["streamResumeError"])
	}
}

// A resume request that keeps failing has to give up exactly when the
// consecutive budget runs out: not on the first failure, and not never.
func TestStreamResumeGivesUpOnRepeatedResumeRequestFailure(t *testing.T) {
	shrinkStreamResumeBackoff(t)
	const chunk = 128 * 1024
	f := &resumeAttemptFixture{total: 1 << 20, chunkBytes: chunk, failResumes: -1}

	rec, req := f.run(t)

	if f.resumes != streamResumeMaxAttempts {
		t.Fatalf("resume requests = %d, want %d", f.resumes, streamResumeMaxAttempts)
	}
	if got := rec.Body.Len(); got != chunk {
		t.Fatalf("delivered %d bytes, want the initial chunk of %d", got, chunk)
	}
	fields := AccessLogFields(req.Context())
	if got := fields["streamResumeAttempts"]; got != streamResumeMaxAttempts {
		t.Fatalf("streamResumeAttempts = %v, want %d", got, streamResumeMaxAttempts)
	}
	msg, _ := fields["streamResumeError"].(string)
	if !strings.Contains(msg, "attempts exhausted") || !strings.Contains(msg, "connection refused") {
		t.Fatalf("streamResumeError = %q, want exhausted attempts naming the transport error", msg)
	}
}

// A validator mismatch means the upstream is now serving different bytes, so
// retrying could only splice the wrong data into the client's stream.
func TestStreamResumeAbortsImmediatelyOnValidatorChange(t *testing.T) {
	shrinkStreamResumeBackoff(t)
	f := &resumeAttemptFixture{total: 1 << 20, chunkBytes: 128 * 1024}
	f.mutateResume = func(h http.Header) { h.Set("ETag", `"media-v2"`) }

	_, req := f.run(t)

	if f.resumes != 1 {
		t.Fatalf("resume requests = %d, want 1 with no retry", f.resumes)
	}
	fields := AccessLogFields(req.Context())
	if got := fields["streamResumeAttempts"]; got != 1 {
		t.Fatalf("streamResumeAttempts = %v, want 1", got)
	}
	msg, _ := fields["streamResumeError"].(string)
	if !strings.Contains(msg, "validator changed") {
		t.Fatalf("streamResumeError = %q, want it to report the changed validator", msg)
	}
	if strings.Contains(msg, "attempts exhausted") {
		t.Fatalf("streamResumeError = %q, want an immediate abort rather than an exhausted budget", msg)
	}
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
