// Package alert 通过通知渠道（Telegram）告诉用户「出事了」：节点故障与恢复、程序内部错误。
// 判定在调用方 goroutine 里只做内存运算，读配置、写状态和发消息都交给后台 worker，
// 探测循环和写日志的调用方不会被网络请求卡住。
package alert

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"embyproxy/internal/localtime"
	"embyproxy/internal/logging"
	"embyproxy/internal/probe"
	"embyproxy/internal/storage"
)

const (
	// recoverAfter 故障中的节点连续成功多少次判定为恢复。
	recoverAfter = 2
	// errorCollectWindow 第一条错误到达后再等这么久，把同一波错误合并成一条消息。
	errorCollectWindow = time.Minute
	// errorCooldown 两条程序错误消息之间的最小间隔，冷却期内的错误计入下一条。
	errorCooldown = 10 * time.Minute

	errorMaxLines     = 5
	errorMaxDistinct  = 50
	errorLineMaxRunes = 120
	queueSize         = 256
	sendTimeout       = 20 * time.Second
	alertUID          = "admin"
)

// Sender 是通知渠道，telegram.Service 实现了它。
type Sender interface {
	Send(ctx context.Context, cfg storage.TGConfig, text string) bool
}

// Service 维护节点的故障状态并聚合程序错误。所有导出方法都可并发调用。
type Service struct {
	store  *storage.Store
	sender Sender
	log    *logging.Logger

	mu    sync.Mutex
	nodes map[string]*nodeState

	events        chan nodeEvent
	errors        chan logging.ErrorRecord
	droppedErrors atomic.Int64

	collectWindow time.Duration
	cooldown      time.Duration
}

// nodeState 一个节点的连续成功/失败计数。down 表示已经判定为故障（无论消息是否发出）。
type nodeState struct {
	fails int
	oks   int
	down  bool
	// since 当前失败连击里第一个失败样本的时间；down 时就是故障开始时间。
	since int64
	// upAt 故障中第一个成功样本的时间，恢复时用来算故障持续多久。
	upAt int64
}

type nodeEventKind int

const (
	nodeDown nodeEventKind = iota + 1
	nodeRecovered
)

type nodeEvent struct {
	kind   nodeEventKind
	node   storage.Node
	sample probe.Sample
	target string
	fails  int
	since  int64
	upAt   int64
}

func New(store *storage.Store, sender Sender, log *logging.Logger) *Service {
	if log == nil {
		log = logging.New("silent", false)
	}
	return &Service{
		store:         store,
		sender:        sender,
		log:           log,
		nodes:         map[string]*nodeState{},
		events:        make(chan nodeEvent, queueSize),
		errors:        make(chan logging.ErrorRecord, queueSize),
		collectWindow: errorCollectWindow,
		cooldown:      errorCooldown,
	}
}

// Start 回填持久化的故障状态、开始接收 ERROR 日志并启动后台 worker，需在探测开始前调用。
func (s *Service) Start(ctx context.Context) {
	if s == nil {
		return
	}
	s.restore(ctx)
	stopObserving := s.log.ObserveErrors(s.ObserveError)
	go func() {
		defer stopObserving()
		s.run(ctx)
	}()
}

// restore 把上次进程里已判定为故障的节点标回故障中：重启后不会再发一次「节点故障」，
// 恢复时仍会发「节点恢复」。
func (s *Service) restore(ctx context.Context) {
	if s.store == nil {
		return
	}
	states, err := s.store.ListNodeAlertStates(ctx, alertUID)
	if err != nil {
		s.log.Warn("alert", "restore node alert states failed", map[string]any{"event": "alertRestoreFailed", "error": err.Error()})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, st := range states {
		s.nodes[name] = &nodeState{down: true, since: st.Since}
	}
}

// ObserveNode 实现 probe.Observer：用节点整体样本推进故障状态机，发生状态切换时交给 worker。
func (s *Service) ObserveNode(node storage.Node, sample probe.Sample, target string) {
	if s == nil || node.Name == "" {
		return
	}
	threshold := s.failThreshold()
	s.mu.Lock()
	st := s.nodes[node.Name]
	if st == nil {
		st = &nodeState{}
		s.nodes[node.Name] = st
	}
	var ev *nodeEvent
	if sample.OK {
		st.fails = 0
		if st.down {
			if st.oks == 0 {
				st.upAt = sample.At
			}
			st.oks++
			if st.oks >= recoverAfter {
				ev = &nodeEvent{kind: nodeRecovered, since: st.since, upAt: st.upAt}
				*st = nodeState{}
			}
		}
	} else {
		st.oks = 0
		if st.fails == 0 && !st.down {
			st.since = sample.At
		}
		st.fails++
		if !st.down && st.fails >= threshold {
			st.down = true
			ev = &nodeEvent{kind: nodeDown, fails: st.fails, since: st.since}
		}
	}
	s.mu.Unlock()
	if ev == nil {
		return
	}
	ev.node, ev.sample, ev.target = node, sample, target
	select {
	case s.events <- *ev:
	default:
		s.log.Warn("alert", "node alert queue full", map[string]any{"event": "nodeAlertDropped", "node": node.Name})
	}
}

// RetainNodes 实现 probe.Observer：丢掉已删除或改名节点的内存状态。持久化状态由 DeleteNode 清理。
func (s *Service) RetainNodes(names []string) {
	if s == nil {
		return
	}
	keep := make(map[string]bool, len(names))
	for _, name := range names {
		keep[name] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for name := range s.nodes {
		if !keep[name] {
			delete(s.nodes, name)
		}
	}
}

func (s *Service) failThreshold() int {
	if s.store == nil {
		return storage.DefaultAlertFailThreshold
	}
	cfg, err := s.store.GetTGConfig(context.Background())
	if err != nil {
		return storage.DefaultAlertFailThreshold
	}
	return cfg.AlertFailThreshold
}

// ObserveError 接收 ERROR 日志（logging.ObserveErrors 的回调），只做过滤和非阻塞入队。
func (s *Service) ObserveError(rec logging.ErrorRecord) {
	if s == nil || !shouldAlert(rec) {
		return
	}
	select {
	case s.errors <- rec:
	default:
		// 队列满说明错误正在刷屏，计数即可，消息里的总条数仍然准确。
		s.droppedErrors.Add(1)
	}
}

// shouldAlert 判断一条 ERROR 是否算「程序故障」。不告警的有两类：
//   - 上游或客户端造成的日常失败：access 作用域（5xx 访问日志）、proxy/direct 的
//     requestFailed（所有线路都失败）与 upstreamReady（上游返回 5xx），节点故障告警已覆盖；
//   - 通知链路自身：telegram/alert 作用域与调度器的 reportError，用同一渠道报告只会形成回环。
//
// 其余 ERROR（数据库读写、文件读写、调度器 panic、配置加载失败等）都会告警。
func shouldAlert(rec logging.ErrorRecord) bool {
	switch rec.Scope {
	case "access", "telegram", "alert":
		return false
	}
	switch rec.Event {
	case "requestFailed", "upstreamReady", "reportError":
		return false
	}
	return true
}

func (s *Service) run(ctx context.Context) {
	var batch errorBatch
	var lastErrorSent time.Time
	var timer *time.Timer
	var flush <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-s.events:
			s.handleNodeEvent(ctx, ev)
		case rec := <-s.errors:
			batch.add(rec)
			if flush != nil {
				continue
			}
			at := batch.first.Add(s.collectWindow)
			if !lastErrorSent.IsZero() {
				if next := lastErrorSent.Add(s.cooldown); next.After(at) {
					at = next
				}
			}
			timer = time.NewTimer(time.Until(at))
			flush = timer.C
		case now := <-flush:
			timer, flush = nil, nil
			batch.total += int(s.droppedErrors.Swap(0))
			if s.flushErrors(ctx, batch, now) {
				lastErrorSent = now
			}
			batch = errorBatch{}
		}
	}
}

func (s *Service) handleNodeEvent(ctx context.Context, ev nodeEvent) {
	switch ev.kind {
	case nodeDown:
		s.log.Info("alert", "node down", map[string]any{"event": "nodeDown", "node": ev.node.Name, "count": ev.fails, "error": ev.sample.Err})
	case nodeRecovered:
		s.log.Info("alert", "node recovered", map[string]any{"event": "nodeRecovered", "node": ev.node.Name, "target": logging.FormatTarget(ev.target)})
	}
	if s.store == nil {
		return
	}
	cfg, err := s.store.GetTGConfig(ctx)
	if err != nil {
		s.log.Warn("alert", "load notify config failed", map[string]any{"event": "alertConfigFailed", "error": err.Error()})
		return
	}
	switch ev.kind {
	case nodeDown:
		// 没开告警时不记状态：用户从没收到「节点故障」，恢复时也就不该冒出一条「节点恢复」。
		if !cfg.NodeAlertsOn() {
			return
		}
		state := storage.NodeAlertState{Since: ev.since, DownAt: ev.sample.At}
		if err := s.store.SaveNodeAlertState(ctx, alertUID, ev.node.Name, state); err != nil {
			s.log.Warn("alert", "save node alert state failed", map[string]any{"event": "alertStateSaveFailed", "node": ev.node.Name, "error": err.Error()})
		}
		s.send(ctx, cfg, downText(ev), "nodeDownAlertFailed")
	case nodeRecovered:
		state, ok, err := s.store.GetNodeAlertState(ctx, alertUID, ev.node.Name)
		if err != nil {
			s.log.Warn("alert", "load node alert state failed", map[string]any{"event": "alertStateLoadFailed", "node": ev.node.Name, "error": err.Error()})
			return
		}
		if !ok {
			return
		}
		if err := s.store.DeleteNodeAlertState(ctx, alertUID, ev.node.Name); err != nil {
			s.log.Warn("alert", "delete node alert state failed", map[string]any{"event": "alertStateDeleteFailed", "node": ev.node.Name, "error": err.Error()})
		}
		if !cfg.NodeAlertsOn() {
			return
		}
		if ev.since <= 0 {
			ev.since = state.Since
		}
		s.send(ctx, cfg, recoveredText(ev), "nodeRecoveredAlertFailed")
	}
}

// flushErrors 发出一条聚合后的程序错误消息，返回是否尝试了发送（用于冷却计时）。
// 发送失败只记日志、不重试，避免网络故障时反复打 Telegram 接口。
func (s *Service) flushErrors(ctx context.Context, batch errorBatch, now time.Time) bool {
	if batch.total == 0 || s.store == nil {
		return false
	}
	cfg, err := s.store.GetTGConfig(ctx)
	if err != nil || !cfg.ErrorAlertsOn() {
		return false
	}
	s.send(ctx, cfg, batch.text(now), "errorAlertFailed")
	return true
}

func (s *Service) send(ctx context.Context, cfg storage.TGConfig, text, failEvent string) {
	if s.sender == nil {
		return
	}
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
	defer cancel()
	if !s.sender.Send(sendCtx, cfg, text) {
		s.log.Warn("alert", "alert send failed", map[string]any{"event": failEvent})
	}
}

func downText(ev nodeEvent) string {
	return strings.Join([]string{
		fmt.Sprintf("🔴 节点故障：%s（/%s）", displayName(ev.node), ev.node.Name),
		fmt.Sprintf("所有上游线路均不可用，已连续 %d 次检测失败", ev.fails),
		"最近错误：" + errorPhrase(ev.sample),
		"开始时间：" + localtime.FormatUnixMilli(ev.since, "15:04"),
	}, "\n")
}

func recoveredText(ev nodeEvent) string {
	return strings.Join([]string{
		fmt.Sprintf("🟢 节点恢复：%s（/%s）", displayName(ev.node), ev.node.Name),
		"故障持续 " + formatDuration(time.Duration(ev.upAt-ev.since)*time.Millisecond),
		fmt.Sprintf("当前线路：%s · %d ms", targetHost(ev.target), ev.sample.MS),
	}, "\n")
}

func displayName(node storage.Node) string {
	if name := strings.TrimSpace(node.DisplayName); name != "" {
		return name
	}
	return node.Name
}

func errorPhrase(sample probe.Sample) string {
	if sample.Err != "" {
		return sample.Err
	}
	if sample.Status > 0 {
		return fmt.Sprintf("HTTP %d", sample.Status)
	}
	return "未知错误"
}

func targetHost(target string) string {
	if u, err := url.Parse(target); err == nil && u.Host != "" {
		return u.Host
	}
	return target
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return "不到 1 分钟"
	}
	minutes := int64(d / time.Minute)
	days, hours, mins := minutes/1440, minutes%1440/60, minutes%60
	switch {
	case days > 0:
		return fmt.Sprintf("%d 天 %d 小时", days, hours)
	case hours > 0:
		return fmt.Sprintf("%d 小时 %d 分钟", hours, mins)
	default:
		return fmt.Sprintf("%d 分钟", mins)
	}
}

// errorBatch 一波待发送的程序错误，按 [scope] message 去重，保留首次出现的顺序。
type errorBatch struct {
	first  time.Time
	total  int
	keys   []string
	counts map[string]int
}

func (b *errorBatch) add(rec logging.ErrorRecord) {
	if b.total == 0 {
		b.first = rec.Time
		if b.first.IsZero() {
			b.first = time.Now()
		}
	}
	b.total++
	line := "[" + rec.Scope + "] " + strings.TrimSpace(rec.Message)
	if rec.Error != "" {
		line += "：" + rec.Error
	}
	line = truncateRunes(strings.Join(strings.Fields(line), " "), errorLineMaxRunes)
	if b.counts == nil {
		b.counts = map[string]int{}
	}
	if _, seen := b.counts[line]; !seen {
		// 种类太多时只计总数，别让一次刷屏把内存撑大。
		if len(b.keys) >= errorMaxDistinct {
			return
		}
		b.keys = append(b.keys, line)
	}
	b.counts[line]++
}

func (b errorBatch) text(now time.Time) string {
	minutes := int((now.Sub(b.first) + time.Minute - 1) / time.Minute)
	if minutes < 1 {
		minutes = 1
	}
	lines := []string{fmt.Sprintf("⚠️ 程序错误：近 %d 分钟 %d 条", minutes, b.total)}
	for i, key := range b.keys {
		if i >= errorMaxLines {
			lines = append(lines, fmt.Sprintf("…另有 %d 种错误，详见控制台日志", len(b.keys)-errorMaxLines))
			break
		}
		if n := b.counts[key]; n > 1 {
			key += fmt.Sprintf(" ×%d", n)
		}
		lines = append(lines, key)
	}
	return strings.Join(lines, "\n")
}

func truncateRunes(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max]) + "…"
}
