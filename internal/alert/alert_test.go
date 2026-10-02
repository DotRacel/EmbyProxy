package alert

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"embyproxy/internal/localtime"
	"embyproxy/internal/logging"
	"embyproxy/internal/probe"
	"embyproxy/internal/storage"
)

type fakeSender struct {
	ch chan string
}

func newFakeSender() *fakeSender {
	return &fakeSender{ch: make(chan string, 16)}
}

func (f *fakeSender) Send(_ context.Context, _ storage.TGConfig, text string) bool {
	f.ch <- text
	return true
}

func (f *fakeSender) expect(t *testing.T) string {
	t.Helper()
	select {
	case text := <-f.ch:
		return text
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an alert")
		return ""
	}
}

func (f *fakeSender) expectNone(t *testing.T, wait time.Duration) {
	t.Helper()
	select {
	case text := <-f.ch:
		t.Fatalf("unexpected alert: %q", text)
	case <-time.After(wait):
	}
}

func newAlertTestStore(t *testing.T, cfg storage.TGConfig) *storage.Store {
	t.Helper()
	store, err := storage.New(filepath.Join(t.TempDir(), "alert.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SaveTGConfig(context.Background(), cfg); err != nil {
		t.Fatalf("SaveTGConfig() error = %v", err)
	}
	return store
}

func enabledConfig() storage.TGConfig {
	cfg := storage.DefaultTGConfig()
	cfg.Enabled, cfg.Token, cfg.Chat = true, "token", "chat"
	return cfg
}

func startService(t *testing.T, store *storage.Store, sender Sender, log *logging.Logger) (*Service, context.CancelFunc) {
	t.Helper()
	if log == nil {
		log = logging.New("silent", false)
	}
	svc := New(store, sender, log)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	svc.Start(ctx)
	return svc, cancel
}

const testTarget = "https://line-a.example.com:8920"

var (
	testNode = storage.Node{Name: "alpha", DisplayName: "朋友服", Target: testTarget}
	baseAt   = time.Date(2026, 10, 1, 8, 30, 0, 0, localtime.Location()).UnixMilli()
)

func failAt(minute int) probe.Sample {
	return probe.Sample{At: baseAt + int64(minute)*60_000, MS: 8000, Err: "超时"}
}

func okAt(minute int, ms int64) probe.Sample {
	return probe.Sample{At: baseAt + int64(minute)*60_000, MS: ms, Status: 200, OK: true}
}

func TestNodeAlertFiresOnceOnDownAndOnceOnRecovery(t *testing.T) {
	store := newAlertTestStore(t, enabledConfig())
	sender := newFakeSender()
	svc, _ := startService(t, store, sender, nil)

	svc.ObserveNode(testNode, failAt(0), testTarget)
	svc.ObserveNode(testNode, failAt(1), testTarget)
	sender.expectNone(t, 50*time.Millisecond)

	svc.ObserveNode(testNode, failAt(2), testTarget)
	down := sender.expect(t)
	for _, want := range []string{
		"🔴 节点故障：朋友服（/alpha）",
		"所有上游线路均不可用，已连续 3 次检测失败",
		"最近错误：超时",
		"开始时间：08:30",
	} {
		if !strings.Contains(down, want) {
			t.Fatalf("down alert = %q, want %q", down, want)
		}
	}
	if _, ok, _ := store.GetNodeAlertState(context.Background(), "admin", "alpha"); !ok {
		t.Fatal("down state should be persisted")
	}

	// 故障中继续失败、偶尔成功一次都不再发消息。
	svc.ObserveNode(testNode, failAt(3), testTarget)
	svc.ObserveNode(testNode, okAt(4, 100), testTarget)
	svc.ObserveNode(testNode, failAt(5), testTarget)
	svc.ObserveNode(testNode, okAt(6, 110), testTarget)
	sender.expectNone(t, 50*time.Millisecond)

	svc.ObserveNode(testNode, okAt(7, 130), testTarget)
	up := sender.expect(t)
	for _, want := range []string{
		"🟢 节点恢复：朋友服（/alpha）",
		"故障持续 6 分钟",
		"当前线路：line-a.example.com:8920 · 130 ms",
	} {
		if !strings.Contains(up, want) {
			t.Fatalf("recovered alert = %q, want %q", up, want)
		}
	}
	deadline := time.Now().Add(time.Second)
	for {
		if _, ok, _ := store.GetNodeAlertState(context.Background(), "admin", "alpha"); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("down state should be cleared after recovery")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 恢复后再次故障是新的一轮，重新计数。
	svc.ObserveNode(testNode, failAt(8), testTarget)
	svc.ObserveNode(testNode, failAt(9), testTarget)
	sender.expectNone(t, 30*time.Millisecond)
	svc.ObserveNode(testNode, failAt(10), testTarget)
	if again := sender.expect(t); !strings.Contains(again, "开始时间：08:38") {
		t.Fatalf("second outage alert = %q", again)
	}
}

func TestNodeAlertStateSurvivesRestart(t *testing.T) {
	store := newAlertTestStore(t, enabledConfig())
	sender := newFakeSender()
	first, stop := startService(t, store, sender, nil)
	for i := 0; i < 3; i++ {
		first.ObserveNode(testNode, failAt(i), testTarget)
	}
	sender.expect(t)
	stop()

	// 新进程：内存状态为空，靠持久化状态知道节点仍在故障中。
	second, _ := startService(t, store, sender, nil)
	for i := 3; i < 8; i++ {
		second.ObserveNode(testNode, failAt(i), testTarget)
	}
	sender.expectNone(t, 50*time.Millisecond)

	second.ObserveNode(testNode, okAt(20, 90), testTarget)
	second.ObserveNode(testNode, okAt(21, 95), testTarget)
	up := sender.expect(t)
	if !strings.Contains(up, "🟢 节点恢复") || !strings.Contains(up, "故障持续 20 分钟") {
		t.Fatalf("recovered alert after restart = %q", up)
	}
}

func TestNodeAlertsFollowConfig(t *testing.T) {
	cfg := enabledConfig()
	cfg.AlertNodes = false
	store := newAlertTestStore(t, cfg)
	sender := newFakeSender()
	svc, _ := startService(t, store, sender, nil)

	for i := 0; i < 5; i++ {
		svc.ObserveNode(testNode, failAt(i), testTarget)
	}
	sender.expectNone(t, 50*time.Millisecond)
	if _, ok, _ := store.GetNodeAlertState(context.Background(), "admin", "alpha"); ok {
		t.Fatal("no state should be kept while node alerts are off")
	}

	// 故障期间打开告警：用户没收到过「节点故障」，恢复时也不该冒出「节点恢复」。
	cfg.AlertNodes = true
	cfg.AlertFailThreshold = 1
	if err := store.SaveTGConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	svc.ObserveNode(testNode, okAt(6, 80), testTarget)
	svc.ObserveNode(testNode, okAt(7, 80), testTarget)
	sender.expectNone(t, 50*time.Millisecond)

	// 阈值 1：一次失败就判定故障。
	other := storage.Node{Name: "beta", Target: testTarget}
	svc.ObserveNode(other, probe.Sample{At: baseAt, Status: 502, Err: "502 Bad Gateway"}, testTarget)
	if down := sender.expect(t); !strings.Contains(down, "🔴 节点故障：beta（/beta）") || !strings.Contains(down, "已连续 1 次") || !strings.Contains(down, "最近错误：502 Bad Gateway") {
		t.Fatalf("threshold-1 alert = %q", down)
	}

	// 通知整体关闭时什么都不发。
	cfg.Enabled = false
	if err := store.SaveTGConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	svc.ObserveNode(storage.Node{Name: "gamma", Target: testTarget}, failAt(0), testTarget)
	sender.expectNone(t, 50*time.Millisecond)
}

func TestRetainNodesDropsRemovedNodeState(t *testing.T) {
	store := newAlertTestStore(t, enabledConfig())
	sender := newFakeSender()
	svc, _ := startService(t, store, sender, nil)
	svc.ObserveNode(testNode, failAt(0), testTarget)
	svc.ObserveNode(testNode, failAt(1), testTarget)
	svc.RetainNodes([]string{"other"})
	// 计数已清零，再失败一次不应凑够 3 次。
	svc.ObserveNode(testNode, failAt(2), testTarget)
	sender.expectNone(t, 50*time.Millisecond)
}

func TestErrorAlertsAggregateDedupeAndRateLimit(t *testing.T) {
	store := newAlertTestStore(t, enabledConfig())
	sender := newFakeSender()
	log := logging.New("silent", false)
	svc := New(store, sender, log)
	svc.collectWindow = 40 * time.Millisecond
	svc.cooldown = 400 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)

	log.Error("storage", "write failed", map[string]any{"event": "dbWriteFailed", "error": "disk full"})
	log.Error("storage", "write failed", map[string]any{"event": "dbWriteFailed", "error": "disk full"})
	log.Error("scheduler", "cleanup panic", map[string]any{"event": "cleanupPanic"})
	// 以下都是上游/客户端问题或通知链路自身，不应计入。
	log.Error("access", "request finished", map[string]any{"event": "requestFinished", "status": 502})
	log.Error("proxy", "request failed", map[string]any{"event": "requestFailed", "error": "all targets failed"})
	log.Error("proxy", "response ready", map[string]any{"event": "upstreamReady", "status": 503})
	log.Error("telegram", "send failed", nil)
	log.Error("scheduler", "report error", map[string]any{"event": "reportError", "error": "db locked"})

	text := sender.expect(t)
	lines := strings.Split(text, "\n")
	if lines[0] != "⚠️ 程序错误：近 1 分钟 3 条" {
		t.Fatalf("header = %q (full %q)", lines[0], text)
	}
	if len(lines) != 3 || lines[1] != "[storage] write failed：disk full ×2" || lines[2] != "[scheduler] cleanup panic" {
		t.Fatalf("lines = %q", lines)
	}

	// 冷却期内的错误攒到下一条，冷却结束后才发。
	log.Error("proxy", "node lookup failed", map[string]any{"event": "nodeLookupFailed", "error": "database is locked"})
	sender.expectNone(t, 150*time.Millisecond)
	next := sender.expect(t)
	if !strings.HasPrefix(next, "⚠️ 程序错误：近 1 分钟 1 条\n[proxy] node lookup failed：database is locked") {
		t.Fatalf("cooldown alert = %q", next)
	}
}

func TestErrorAlertsRespectConfig(t *testing.T) {
	cfg := enabledConfig()
	cfg.AlertErrors = false
	store := newAlertTestStore(t, cfg)
	sender := newFakeSender()
	log := logging.New("silent", false)
	svc := New(store, sender, log)
	svc.collectWindow = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)

	log.Error("storage", "write failed", nil)
	sender.expectNone(t, 100*time.Millisecond)
}

func TestShouldAlertExcludesRoutineAndNotifierErrors(t *testing.T) {
	cases := []struct {
		rec  logging.ErrorRecord
		want bool
	}{
		{logging.ErrorRecord{Scope: "access", Event: "requestFinished"}, false},
		{logging.ErrorRecord{Scope: "proxy", Event: "requestFailed"}, false},
		{logging.ErrorRecord{Scope: "proxy", Event: "upstreamReady"}, false},
		{logging.ErrorRecord{Scope: "direct", Event: "upstreamReady"}, false},
		{logging.ErrorRecord{Scope: "telegram"}, false},
		{logging.ErrorRecord{Scope: "alert"}, false},
		{logging.ErrorRecord{Scope: "scheduler", Event: "reportError"}, false},
		{logging.ErrorRecord{Scope: "proxy", Event: "nodeLookupFailed"}, true},
		{logging.ErrorRecord{Scope: "scheduler", Event: "cleanupPanic"}, true},
		{logging.ErrorRecord{Scope: "startup", Event: "serverFailed"}, true},
		{logging.ErrorRecord{Scope: "playback", Event: "playbackStateWriteFailed"}, true},
		{logging.ErrorRecord{Scope: "probe", Event: "probePersistFailed"}, true},
		{logging.ErrorRecord{Scope: "traffic", Event: "captureWriteFailed"}, true},
		{logging.ErrorRecord{Scope: "logging", Event: "consoleLogHistoryWriteFailed"}, true},
	}
	for _, c := range cases {
		if got := shouldAlert(c.rec); got != c.want {
			t.Errorf("shouldAlert(%+v) = %v, want %v", c.rec, got, c.want)
		}
	}
}

func TestErrorBatchTextLimitsLinesAndTruncates(t *testing.T) {
	var b errorBatch
	start := time.Now()
	for i := 0; i < 7; i++ {
		b.add(logging.ErrorRecord{Time: start, Scope: "storage", Message: fmt.Sprintf("failure %d", i)})
	}
	b.add(logging.ErrorRecord{Time: start, Scope: "storage", Message: strings.Repeat("长", 200)})
	text := b.text(start.Add(3*time.Minute + time.Second))
	lines := strings.Split(text, "\n")
	if lines[0] != "⚠️ 程序错误：近 4 分钟 8 条" {
		t.Fatalf("header = %q", lines[0])
	}
	if len(lines) != 1+errorMaxLines+1 || !strings.HasPrefix(lines[len(lines)-1], "…另有 3 种错误") {
		t.Fatalf("lines = %q", lines)
	}

	var long errorBatch
	long.add(logging.ErrorRecord{Time: start, Scope: "storage", Message: strings.Repeat("长", 200)})
	line := strings.Split(long.text(start), "\n")[1]
	if n := len([]rune(line)); n != errorLineMaxRunes+1 || !strings.HasSuffix(line, "…") {
		t.Fatalf("long line has %d runes: %q", n, line)
	}
}

func TestFormatDuration(t *testing.T) {
	cases := map[time.Duration]string{
		30 * time.Second:              "不到 1 分钟",
		5 * time.Minute:               "5 分钟",
		2*time.Hour + 3*time.Minute:   "2 小时 3 分钟",
		26*time.Hour + 59*time.Minute: "1 天 2 小时",
		-1 * time.Minute:              "不到 1 分钟",
	}
	for in, want := range cases {
		if got := formatDuration(in); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", in, got, want)
		}
	}
}
