package probe

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRegistryStatsSummarizesLatestSampleAndAvailability(t *testing.T) {
	reg := NewRegistry()
	now := time.Now().UnixMilli()
	reg.RecordNode("uhdnow", Sample{At: now - 3000, MS: 100, Status: 200, OK: true})
	reg.RecordNode("uhdnow", Sample{At: now - 2000, MS: 0, Err: "超时"})
	reg.RecordNode("uhdnow", Sample{At: now - 1000, MS: 180, Status: 200, OK: true})

	stats := reg.Stats("uhdnow", nil)
	if !stats.Probed || !stats.OK {
		t.Fatalf("expected probed and ok, got %+v", stats)
	}
	if stats.LastMS != 180 {
		t.Fatalf("LastMS = %d, want 180", stats.LastMS)
	}
	if stats.Samples != 3 {
		t.Fatalf("Samples = %d, want 3", stats.Samples)
	}
	if want := 2.0 / 3.0; stats.Availability < want-0.001 || stats.Availability > want+0.001 {
		t.Fatalf("Availability = %v, want %v", stats.Availability, want)
	}
	if stats.AvgMS != 140 {
		t.Fatalf("AvgMS = %d, want 140", stats.AvgMS)
	}
	if stats.MaxMS != 180 {
		t.Fatalf("MaxMS = %d, want 180", stats.MaxMS)
	}
}

func TestRegistryStatsReportsTargetsInConfiguredOrder(t *testing.T) {
	reg := NewRegistry()
	now := time.Now().UnixMilli()
	reg.RecordTarget("uhdnow", "https://v1.example.com", Sample{At: now, MS: 140, Status: 200, OK: true})
	reg.RecordTarget("uhdnow", "https://v2.example.com", Sample{At: now, MS: 8000, Err: "超时"})

	stats := reg.Stats("uhdnow", []string{"https://v1.example.com", "https://v2.example.com", "https://v3.example.com"})
	if len(stats.Targets) != 3 {
		t.Fatalf("len(Targets) = %d, want 3", len(stats.Targets))
	}
	if !stats.Targets[0].Primary || !stats.Targets[0].OK || stats.Targets[0].MS != 140 {
		t.Fatalf("primary target = %+v", stats.Targets[0])
	}
	if stats.Targets[1].OK || stats.Targets[1].Err != "超时" {
		t.Fatalf("secondary target = %+v", stats.Targets[1])
	}
	// 从未探测过的线路应给出 -1，而不是让前端把 0ms 当成极快。
	if stats.Targets[2].MS != -1 || stats.Targets[2].Samples != 0 {
		t.Fatalf("unprobed target = %+v", stats.Targets[2])
	}
}

func TestRegistryDropsSamplesOlderThanRetention(t *testing.T) {
	reg := NewRegistry()
	now := time.Now().UnixMilli()
	reg.RecordNode("lab", Sample{At: now - Retention.Milliseconds() - 60_000, MS: 10, OK: true})
	reg.RecordNode("lab", Sample{At: now, MS: 20, OK: true})

	if stats := reg.Stats("lab", nil); stats.Samples != 1 {
		t.Fatalf("Samples = %d, want 1 (expired sample should be dropped)", stats.Samples)
	}
}

func TestRegistrySeriesBucketsSamplesAndMarksEmptyBuckets(t *testing.T) {
	reg := NewRegistry()
	now := time.Now().UnixMilli()
	// 两个样本都落在最近一小时窗口的末尾。
	reg.RecordNode("uhdnow", Sample{At: now - 1000, MS: 100, OK: true})
	reg.RecordNode("uhdnow", Sample{At: now - 500, MS: 300, OK: true})

	points := reg.Series("uhdnow", time.Hour, 12)
	if len(points) != 12 {
		t.Fatalf("len(points) = %d, want 12", len(points))
	}
	last := points[len(points)-1]
	if last.MS != 200 {
		t.Fatalf("last bucket MS = %d, want 200 (average)", last.MS)
	}
	if points[0].MS != -1 || points[0].OK != 0 {
		t.Fatalf("empty bucket = %+v, want MS -1 and OK 0", points[0])
	}
}

func TestRegistryRetainDropsRemovedNodes(t *testing.T) {
	reg := NewRegistry()
	reg.RecordNode("keep", Sample{At: time.Now().UnixMilli(), MS: 5, OK: true})
	reg.RecordNode("gone", Sample{At: time.Now().UnixMilli(), MS: 5, OK: true})
	reg.RecordTarget("gone", "https://x.example.com", Sample{At: time.Now().UnixMilli(), MS: 5, OK: true})

	reg.Retain([]string{"keep"})

	if stats := reg.Stats("gone", nil); stats.Probed {
		t.Fatal("expected removed node samples to be dropped")
	}
	if stats := reg.Stats("keep", nil); !stats.Probed {
		t.Fatal("expected retained node samples to survive")
	}
}

func TestSparkDownsamplesAndMarksFailures(t *testing.T) {
	samples := make([]Sample, 0, 24)
	for i := 0; i < 24; i++ {
		samples = append(samples, Sample{At: int64(i), MS: 100, OK: true})
	}
	// 把最后两个样本标为失败，最后一个 spark 点应为 -1。
	samples[22].OK, samples[23].OK = false, false

	out := spark(samples, 12)
	if len(out) != 12 {
		t.Fatalf("len(spark) = %d, want 12", len(out))
	}
	if out[0] != 100 {
		t.Fatalf("spark[0] = %d, want 100", out[0])
	}
	if out[11] != -1 {
		t.Fatalf("spark[11] = %d, want -1 (all-failed bucket)", out[11])
	}
}

func TestProbeTargetRecordsStatusAndLatency(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != probePath {
			t.Errorf("probe path = %q, want %q", r.URL.Path, probePath)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	p := NewProber(NewRegistry(), nil, nil)
	sample := p.probeTarget(t.Context(), server.URL)
	if !sample.OK || sample.Status != http.StatusOK {
		t.Fatalf("sample = %+v, want OK 200", sample)
	}
}

func TestProbeTargetMarksNon2xxAsFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	p := NewProber(NewRegistry(), nil, nil)
	sample := p.probeTarget(t.Context(), server.URL)
	if sample.OK {
		t.Fatalf("sample = %+v, want failure", sample)
	}
	if sample.Status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", sample.Status)
	}
}

func TestErrorTextCompressesCommonNetworkFailures(t *testing.T) {
	cases := []struct{ in, want string }{
		{`Get "http://x/y": dial tcp 127.0.0.1:9999: connect: connection refused`, "拒绝连接"},
		{`Get "http://x/y": context deadline exceeded (Client.Timeout exceeded)`, "超时"},
		{`Get "http://x/y": dial tcp: lookup nope.invalid: no such host`, "域名解析失败"},
		{`Get "https://x/y": tls: failed to verify certificate`, "证书校验失败"},
		{`Get "http://x/y": read tcp 1.2.3.4:80: connection reset by peer`, "连接被重置"},
	}
	for _, c := range cases {
		if got := errorText(errors.New(c.in)); got != c.want {
			t.Errorf("errorText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if errorText(nil) != "" {
		t.Error("errorText(nil) should be empty")
	}
}
