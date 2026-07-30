package proxy

import (
	"fmt"
	"net/http"
	"testing"
)

// discardResponseWriter throws the body away so a large-transfer benchmark measures
// the copy path rather than the cost of buffering the response in a recorder.
type discardResponseWriter struct {
	header http.Header
	bytes  int64
}

func (w *discardResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *discardResponseWriter) Write(chunk []byte) (int, error) {
	w.bytes += int64(len(chunk))
	return len(chunk), nil
}

func (w *discardResponseWriter) WriteHeader(int) {}

// BenchmarkRawStreamThroughput measures a large media transfer end to end through
// the /__raw__ route, the path traffic accounting was added to. It exists to answer
// "does counting bytes slow playback down": the counters ride the existing
// io.CopyBuffer path and cost one int64 add per chunk, and registering the stat
// costs one struct store per response, so the numbers should not move measurably
// when the accounting is removed.
func BenchmarkRawStreamThroughput(b *testing.B) {
	const size = 64 << 20
	payload := make([]byte, size)
	f := newTrafficStatsHarness(b, func(req *http.Request) (*http.Response, error) {
		return bytesResponse(http.StatusOK, payload, http.Header{
			"Content-Type":   []string{"video/x-matroska"},
			"Accept-Ranges":  []string{"bytes"},
			"Content-Length": []string{fmt.Sprint(size)},
		}), nil
	})

	b.SetBytes(size)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := &discardResponseWriter{}
		f.getInto(b, "http://8.8.8.8/video.mkv", nil, w)
		if w.bytes != size {
			b.Fatalf("delivered %d bytes, want %d", w.bytes, size)
		}
	}
}
