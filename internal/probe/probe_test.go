package probe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"embyproxy/internal/storage"
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

func TestProbeAllPersistsSamplesAndRestoreRebuildsSeries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx := t.Context()
	store, err := storage.New(filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SaveNode(ctx, "admin", storage.Node{Name: "uhdnow", Target: server.URL}); err != nil {
		t.Fatal(err)
	}

	NewProber(NewRegistry(), store, nil).ProbeAll(ctx)

	samples, err := store.LoadProbeSamples(ctx, Retention)
	if err != nil {
		t.Fatal(err)
	}
	// 一条上游线路 → 一条线路样本 + 一条节点整体样本。
	if len(samples) != 2 {
		t.Fatalf("persisted samples = %d, want 2: %+v", len(samples), samples)
	}

	// 新进程从零开始，回填后详情页的曲线与上游线路延迟应立刻可用。
	fresh := NewProber(NewRegistry(), store, nil)
	fresh.Restore(ctx)
	stats := fresh.registry.Stats("uhdnow", []string{server.URL})
	if !stats.Probed || !stats.OK || stats.Samples != 1 {
		t.Fatalf("restored stats = %+v, want 1 ok sample", stats)
	}
	if len(stats.Targets) != 1 || !stats.Targets[0].OK || stats.Targets[0].Samples != 1 {
		t.Fatalf("restored targets = %+v", stats.Targets)
	}
}

func TestProbeAllDropsSamplesOfRemovedNodes(t *testing.T) {
	ctx := t.Context()
	store, err := storage.New(filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SaveNode(ctx, "admin", storage.Node{Name: "kept", Target: "http://127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendProbeSamples(ctx, []storage.ProbeSample{
		{Node: "removed", At: time.Now().UnixMilli() - 1000, MS: 100, OK: true},
	}); err != nil {
		t.Fatal(err)
	}

	NewProber(NewRegistry(), store, nil).ProbeAll(ctx)

	samples, err := store.LoadProbeSamples(ctx, Retention)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range samples {
		if s.Node == "removed" {
			t.Fatalf("removed node still has samples: %+v", samples)
		}
	}
}

func TestRegistryStatsReportsPerTargetAvailabilityAndAverage(t *testing.T) {
	reg := NewRegistry()
	now := time.Now().UnixMilli()
	target := "https://v1.example.com"
	reg.RecordTarget("uhdnow", target, Sample{At: now - 3000, MS: 100, Status: 200, OK: true})
	reg.RecordTarget("uhdnow", target, Sample{At: now - 2000, MS: 8000, Err: "超时"})
	reg.RecordTarget("uhdnow", target, Sample{At: now - 1000, MS: 300, Status: 200, OK: true})
	reg.RecordTarget("uhdnow", target, Sample{At: now, MS: 0, Err: "拒绝连接"})

	stats := reg.Stats("uhdnow", []string{target, "https://v2.example.com"})
	got := stats.Targets[0]
	if got.Samples != 4 || got.OKSamples != 2 {
		t.Fatalf("samples = %d ok = %d, want 4 and 2", got.Samples, got.OKSamples)
	}
	if got.Availability != 0.5 {
		t.Fatalf("Availability = %v, want 0.5", got.Availability)
	}
	if got.AvgMS != 200 {
		t.Fatalf("AvgMS = %d, want 200 (failed samples excluded)", got.AvgMS)
	}
	if got.OK || got.Err != "拒绝连接" {
		t.Fatalf("latest sample should drive ok/err: %+v", got)
	}
	// 没有样本的线路三个新字段都是 0。
	if empty := stats.Targets[1]; empty.Availability != 0 || empty.OKSamples != 0 || empty.AvgMS != 0 {
		t.Fatalf("unprobed target = %+v", empty)
	}
	if stats.ActiveTarget != "" {
		t.Fatalf("ActiveTarget = %q, want empty (filled by admin)", stats.ActiveTarget)
	}
}

func TestRegistrySeriesCountsSamplesAndKeepsLatestError(t *testing.T) {
	reg := NewRegistry()
	now := time.Now().UnixMilli()
	reg.RecordNode("uhdnow", Sample{At: now - 3000, MS: 0, Err: "超时"})
	reg.RecordNode("uhdnow", Sample{At: now - 2000, MS: 0, Err: "拒绝连接"})
	reg.RecordNode("uhdnow", Sample{At: now - 1000, MS: 120, OK: true})

	points := reg.Series("uhdnow", time.Hour, 12)
	last := points[len(points)-1]
	if last.N != 3 || last.Err != "拒绝连接" || last.MS != 120 {
		t.Fatalf("last bucket = %+v, want n=3 err=拒绝连接 ms=120", last)
	}
	// 无采样（n=0）与全部失败（n>0 且 ok=0）必须能区分开。
	if points[0].N != 0 || points[0].Err != "" {
		t.Fatalf("empty bucket = %+v", points[0])
	}

	reg.RecordNode("down", Sample{At: now - 500, Err: "超时"})
	if p := reg.Series("down", time.Hour, 12)[11]; p.N != 1 || p.OK != 0 || p.MS != -1 || p.Err != "超时" {
		t.Fatalf("failed bucket = %+v", p)
	}
}

func TestRegistrySeriesWithTargetsFollowsConfiguredOrder(t *testing.T) {
	reg := NewRegistry()
	now := time.Now().UnixMilli()
	reg.RecordNode("uhdnow", Sample{At: now - 500, MS: 90, OK: true})
	reg.RecordTarget("uhdnow", "https://b.example.com", Sample{At: now - 500, MS: 90, OK: true})
	reg.RecordTarget("uhdnow", "https://a.example.com", Sample{At: now - 500, Err: "超时"})

	targets := []string{"https://a.example.com", "https://b.example.com"}
	for i := 0; i < targetLimit; i++ {
		targets = append(targets, "https://extra"+string(rune('a'+i))+".example.com")
	}
	points, lines := reg.SeriesWithTargets("uhdnow", targets, time.Hour, 12)
	if len(points) != 12 {
		t.Fatalf("len(points) = %d, want 12", len(points))
	}
	if len(lines) != targetLimit {
		t.Fatalf("len(lines) = %d, want targetLimit %d", len(lines), targetLimit)
	}
	if lines[0].Target != "https://a.example.com" || !lines[0].Primary || lines[1].Primary {
		t.Fatalf("lines order/primary = %+v, %+v", lines[0], lines[1])
	}
	if p := lines[0].Points[11]; p.N != 1 || p.OK != 0 || p.Err != "超时" {
		t.Fatalf("primary last bucket = %+v", p)
	}
	if p := lines[1].Points[11]; p.N != 1 || p.MS != 90 {
		t.Fatalf("secondary last bucket = %+v", p)
	}
	// 各曲线共用同一组时间桶。
	if lines[0].Points[0].At != points[0].At || lines[1].Points[11].At != points[11].At {
		t.Fatal("per-target series must share bucket boundaries with the node series")
	}
	if _, none := reg.SeriesWithTargets("uhdnow", nil, time.Hour, 12); none == nil || len(none) != 0 {
		t.Fatalf("no targets should give an empty (non-nil) slice, got %#v", none)
	}
}

type recordingObserver struct {
	nodes    []string
	targets  []string
	samples  []Sample
	retained [][]string
}

func (o *recordingObserver) ObserveNode(node storage.Node, sample Sample, target string) {
	o.nodes = append(o.nodes, node.Name)
	o.targets = append(o.targets, target)
	o.samples = append(o.samples, sample)
}

func (o *recordingObserver) RetainNodes(names []string) {
	o.retained = append(o.retained, append([]string(nil), names...))
}

func TestProberReportsNodeResultsToObserver(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer down.Close()

	ctx := t.Context()
	store, err := storage.New(filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// 主线挂了、备线可用：节点整体样本应来自备线。
	if err := store.SaveNode(ctx, "admin", storage.Node{Name: "failover", Target: down.URL + "\n" + up.URL}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveNode(ctx, "admin", storage.Node{Name: "dead", Target: down.URL}); err != nil {
		t.Fatal(err)
	}

	obs := &recordingObserver{}
	p := NewProber(NewRegistry(), store, nil)
	p.SetObserver(obs)
	p.ProbeAll(ctx)

	if len(obs.retained) != 1 || len(obs.retained[0]) != 2 {
		t.Fatalf("retained = %v, want one call with both nodes", obs.retained)
	}
	got := map[string]int{}
	for i, name := range obs.nodes {
		got[name] = i
	}
	i := got["failover"]
	if !obs.samples[i].OK || obs.targets[i] != up.URL {
		t.Fatalf("failover result = %+v via %q, want ok via backup", obs.samples[i], obs.targets[i])
	}
	i = got["dead"]
	if obs.samples[i].OK || obs.targets[i] != down.URL || obs.samples[i].Status != http.StatusBadGateway {
		t.Fatalf("dead result = %+v via %q", obs.samples[i], obs.targets[i])
	}

	// 取消的 ctx 下样本都是「已取消」，不应交给告警。
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	before := len(obs.nodes)
	p.ProbeNode(cancelled, storage.Node{Name: "dead", Target: down.URL})
	if len(obs.nodes) != before {
		t.Fatal("cancelled probes must not be observed")
	}
	// 没配上游的节点永远不告警。
	p.ProbeNode(ctx, storage.Node{Name: "empty"})
	if len(obs.nodes) != before {
		t.Fatal("nodes without targets must not be observed")
	}
}
