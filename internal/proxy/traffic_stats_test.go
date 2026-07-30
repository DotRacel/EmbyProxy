package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"embyproxy/internal/config"
	"embyproxy/internal/logging"
	"embyproxy/internal/storage"
)

// trafficStatsHarness serves a media response through the signed /__raw__ route,
// which is how rewritten media source URLs reach this proxy. Everything about it
// is streamed: the round tripper hands back a body, never a buffer the handler
// could measure after the fact.
type trafficStatsHarness struct {
	handler *Handler
	store   *storage.Store
}

func newTrafficStatsHarness(t testing.TB, roundTrip roundTripFunc) *trafficStatsHarness {
	t.Helper()
	store := newProxyTestStore(t)
	node := storage.Node{Name: "node", Secret: "secret", Target: "https://upstream.example"}
	if err := store.SaveNode(context.Background(), "admin", node); err != nil {
		t.Fatal(err)
	}
	h := New(config.Config{}, store, nil, logging.New("silent", false))
	h.rawDirectClient = &http.Client{Transport: roundTrip}
	return &trafficStatsHarness{handler: h, store: store}
}

// get drives one client request through ServeHTTP and returns the delivered body size.
func (f *trafficStatsHarness) get(t testing.TB, raw string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	f.getInto(t, raw, header, rec)
	return rec
}

// getInto drives one client request through ServeHTTP into an arbitrary writer, so
// a benchmark can discard the body instead of buffering gigabytes in a recorder.
func (f *trafficStatsHarness) getInto(t testing.TB, raw string, header http.Header, w http.ResponseWriter) {
	t.Helper()
	link := f.handler.signedRawLink("https://proxy.example", "/node/secret", raw)
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, u.Path+"?"+u.RawQuery, nil)
	req.Header.Set("User-Agent", "TrafficStatsPlayer/1.0")
	for key, values := range header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	f.handler.ServeHTTP(w, req)
}

// traffic waits for the async stats writer to drain and returns the recorded totals.
func (f *trafficStatsHarness) traffic(t testing.TB) (inbound, outbound int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		inbound, outbound = f.trafficNow(t)
		if outbound > 0 || time.Now().After(deadline) {
			return inbound, outbound
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// trafficNow reads the recorded totals without waiting, for assertions that
// expect nothing to have been written.
func (f *trafficStatsHarness) trafficNow(t testing.TB) (inbound, outbound int64) {
	t.Helper()
	rows, err := f.store.GetPlayStats(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		inbound += row.InboundBytes
		outbound += row.OutboundBytes
	}
	return inbound, outbound
}

func mediaHeader(extra map[string]string) http.Header {
	h := http.Header{
		"Content-Type":  []string{"video/x-matroska"},
		"Accept-Ranges": []string{"bytes"},
	}
	for key, value := range extra {
		h.Set(key, value)
	}
	return h
}

// A whole media file pulled through a signed /__raw__ link has to show up in the
// play stats. This is the regression that made playback traffic invisible: only
// handleMediaProxy and handleSTRM registered a PlaybackInput, so the bytes this
// proxy streamed for every rewritten media source URL were counted during the
// copy and then discarded.
func TestRawStreamTrafficRecordsLargeResponseBody(t *testing.T) {
	const size = 8 << 20
	payload := make([]byte, size)
	f := newTrafficStatsHarness(t, func(req *http.Request) (*http.Response, error) {
		return bytesResponse(http.StatusOK, payload, mediaHeader(map[string]string{
			"Content-Length": fmt.Sprint(size),
		})), nil
	})

	rec := f.get(t, "http://8.8.8.8/video.mkv", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.Len(); got != size {
		t.Fatalf("delivered %d bytes, want %d", got, size)
	}
	inbound, outbound := f.traffic(t)
	if inbound != size || outbound != size {
		t.Fatalf("recorded inbound/outbound = %d/%d, want %d/%d", inbound, outbound, size, size)
	}
}

// A seek produces a Range request answered with 206, and only the bytes in that
// range may be counted.
func TestRawStreamTrafficRecordsRangeRequest(t *testing.T) {
	const total = 8 << 20
	const start = 2 << 20
	const length = total - start
	payload := make([]byte, length)
	f := newTrafficStatsHarness(t, func(req *http.Request) (*http.Response, error) {
		if got, want := req.Header.Get("Range"), fmt.Sprintf("bytes=%d-", start); got != want {
			t.Errorf("upstream Range = %q, want %q", got, want)
		}
		return bytesResponse(http.StatusPartialContent, payload, mediaHeader(map[string]string{
			"Content-Length": fmt.Sprint(length),
			"Content-Range":  fmt.Sprintf("bytes %d-%d/%d", start, total-1, total),
		})), nil
	})

	rec := f.get(t, "http://8.8.8.8/video.mkv", http.Header{"Range": []string{fmt.Sprintf("bytes=%d-", start)}})

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusPartialContent)
	}
	if got := rec.Body.Len(); got != length {
		t.Fatalf("delivered %d bytes, want %d", got, length)
	}
	inbound, outbound := f.traffic(t)
	if inbound != length || outbound != length {
		t.Fatalf("recorded inbound/outbound = %d/%d, want %d/%d", inbound, outbound, length, length)
	}
}

// When the upstream drops mid-transfer and the stream is resumed, the bytes
// delivered after the resume are just as real as the ones before it, so the
// recorded total has to cover the whole response rather than the first leg only.
func TestRawStreamTrafficIncludesResumedBytes(t *testing.T) {
	shrinkStreamResumeBackoff(t)
	const total = 1 << 20
	const chunk = 256 * 1024
	dropped := errors.New("connection reset by peer")

	header := func(start int64) http.Header {
		return mediaHeader(map[string]string{
			"ETag":           `"media-v1"`,
			"Content-Range":  fmt.Sprintf("bytes %d-%d/%d", start, int64(total)-1, int64(total)),
			"Content-Length": fmt.Sprint(int64(total) - start),
		})
	}
	newLeg := func(req *http.Request, start int64) *http.Response {
		return &http.Response{
			StatusCode:    http.StatusPartialContent,
			Status:        "206 Partial Content",
			ContentLength: int64(total) - start,
			Header:        header(start),
			Body:          &chunkThenFailBody{remaining: chunk, chunk: chunk, err: dropped},
			Request:       req,
		}
	}
	f := newTrafficStatsHarness(t, func(req *http.Request) (*http.Response, error) {
		var start int64
		if raw := req.Header.Get("Range"); raw != "" {
			if _, err := fmt.Sscanf(raw, "bytes=%d-", &start); err != nil {
				return nil, err
			}
		}
		return newLeg(req, start), nil
	})

	rec := f.get(t, "http://8.8.8.8/video.mkv", http.Header{"Range": []string{"bytes=0-"}})

	if got := rec.Body.Len(); got != total {
		t.Fatalf("delivered %d bytes, want the full %d after resuming", got, total)
	}
	inbound, outbound := f.traffic(t)
	if outbound != total {
		t.Fatalf("recorded outbound = %d, want %d: resumed bytes were dropped", outbound, total)
	}
	if inbound != total {
		t.Fatalf("recorded inbound = %d, want %d: resumed bytes were dropped", inbound, total)
	}
}

// These are playback stats, not a byte meter for every request. A non-media
// resource fetched through the same /__raw__ route must stay out of the totals,
// otherwise the fix would quietly turn the play stats into general HTTP traffic.
func TestRawNonMediaResponseRecordsNoPlaybackTraffic(t *testing.T) {
	payload := make([]byte, 64<<10)
	f := newTrafficStatsHarness(t, func(req *http.Request) (*http.Response, error) {
		return bytesResponse(http.StatusOK, payload, http.Header{
			"Content-Type":   []string{"application/json"},
			"Content-Length": []string{fmt.Sprint(len(payload))},
		}), nil
	})

	rec := f.get(t, "http://8.8.8.8/metadata.json", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	// Give the async writer a chance to record something it should not have.
	time.Sleep(200 * time.Millisecond)
	inbound, outbound := f.trafficNow(t)
	if inbound != 0 || outbound != 0 {
		t.Fatalf("recorded inbound/outbound = %d/%d, want 0/0 for a non-media response", inbound, outbound)
	}
}
