package logging

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"embyproxy/internal/localtime"
)

const (
	DefaultBufferCapacity      = 2000
	DefaultHistoryEntriesFile  = 2000
	DefaultHistoryRotatedFiles = 20
	logHistoryBufferSize       = 64 * 1024
	logHistoryFlushInterval    = time.Second
	// 清理时多扫一段轮转文件，避免上一次运行用了更大的保留文件数时留下孤儿文件。
	// 与 storage.MaxLogHistoryMaxFiles 保持一致。
	logHistorySweepFiles = 200
	// historyFailureInterval 落盘失败的 ERROR 日志最短间隔。磁盘满或目录不可写时每写一行
	// 日志都会失败，不节流的话控制台会被自己的报错刷屏。
	historyFailureInterval = time.Minute
)

var (
	levels = map[string]int{
		"silent": 0,
		"error":  1,
		"warn":   2,
		"info":   3,
		"debug":  4,
	}

	httpURLRE         = regexp.MustCompile(`(?i)^https?://`)
	embeddedHTTPURLRE = regexp.MustCompile(`(?i)https?://[^\s"'<>\\]+`)
	safeLogValueRE    = regexp.MustCompile(`^[A-Za-z0-9_./:@-]+$`)

	metaFieldOrder = map[string]int{
		"event":                10,
		"id":                   20,
		"method":               30,
		"uri":                  40,
		"ip":                   50,
		"node":                 60,
		"nodeTarget":           70,
		"target":               80,
		"upstreamPool":         85,
		"location":             90,
		"range":                100,
		"contentRange":         110,
		"streamResumeFrom":     112,
		"streamResumeAttempts": 114,
		"streamResumeBytes":    116,
		"imageCache":           120,
		"bytes":                130,
		"contentLen":           140,
		"copiedBytes":          150,
		"readBytes":            160,
		"writeBytes":           170,
		"readCalls":            180,
		"writeCalls":           190,
		"responseReadyMs":      200,
		"targetAttemptMs":      210,
		"bodyMs":               220,
		"copyMs":               230,
		"totalMs":              240,
		"firstReadMs":          250,
		"firstReadStatus":      260,
		"lastReadMs":           270,
		"lastWriteMs":          280,
		"upgradeMs":            290,
		"addr":                 300,
		"db":                   310,
		"profile":              320,
		"label":                330,
		"client":               340,
		"version":              350,
		"commit":               360,
		"builtAt":              370,
		"device":               380,
		"deviceId":             390,
		"userAgent":            400,
		"day":                  410,
		"count":                420,
		"reason":               900,
		"side":                 910,
		"contextErr":           920,
		"bodyCopySide":         930,
		"bodyCopyContextErr":   940,
		"bodyCopyError":        950,
		"streamResumeError":    955,
		"error":                960,
	}
)

type Logger struct {
	level     atomic.Int64
	accessLog atomic.Bool
	seq       atomic.Uint64
	// mu keeps buffer and history on the same side of a clear boundary.
	mu      sync.Mutex
	buffer  *logBuffer
	history *logHistory
	subMu   sync.Mutex
	subs    map[chan LogEntry]struct{}
	obsMu   sync.RWMutex
	obsSeq  uint64
	errObs  map[uint64]func(ErrorRecord)
	thrMu   sync.Mutex
	thr     map[string]*throttleState
}

// throttleState 记录一个 scope+event 上次真正写出日志的时间，以及之后被压下的条数。
type throttleState struct {
	last       time.Time
	suppressed int
}

// ErrorRecord 一条 ERROR 日志的结构化摘要，交给 ObserveErrors 注册的观察者。
// Message 与 Error 都已脱敏，可以直接对外发送。
type ErrorRecord struct {
	Time    time.Time
	Scope   string
	Message string
	Event   string
	Error   string
}

type LogEntry struct {
	Type    string `json:"type,omitempty"`
	ID      uint64 `json:"id"`
	Time    string `json:"time"`
	Level   string `json:"level"`
	Scope   string `json:"scope"`
	Message string `json:"message"`
	Line    string `json:"line"`
}

type LogFilter struct {
	Levels map[string]bool
	Query  string
	// Node 与 Query 是两个独立维度，同时给出时取交集。
	// 合成一个 query 的话，节点筛选和搜索词只能二选一下推到服务端，
	// 另一个退化成客户端当前页过滤，分页总数就不准了。
	Node string
}

type LogPage struct {
	Entries      []LogEntry
	HasOlder     bool
	History      bool
	Page         int
	TotalPages   int
	TotalEntries int
}

type logBuffer struct {
	mu       sync.Mutex
	next     uint64
	start    int
	entries  []LogEntry
	capacity int
}

type logHistory struct {
	mu              sync.Mutex
	path            string
	entriesPerFile  int
	maxFiles        int
	entryCount      int
	retainedEntries int
	oldestID        uint64
	file            *os.File
	writer          *bufio.Writer
	closed          bool
	done            chan struct{}
	// onFlushError 后台定时刷盘失败时回调（不持有 mu），由 Logger 转成节流后的 ERROR。
	onFlushError func(error)
}

// historyRotateError 标记 Append 里轮转文件这一步失败，便于和普通写入失败区分开。
type historyRotateError struct{ err error }

func (e historyRotateError) Error() string { return "rotate: " + e.err.Error() }
func (e historyRotateError) Unwrap() error { return e.err }

func (f LogFilter) empty() bool {
	return len(f.Levels) == 0 && strings.TrimSpace(f.Query) == "" && strings.TrimSpace(f.Node) == ""
}

func (f LogFilter) match(e LogEntry) bool {
	if len(f.Levels) > 0 && !f.Levels[strings.ToLower(e.Level)] {
		return false
	}
	line := strings.ToLower(e.Line)
	query := strings.TrimSpace(strings.ToLower(f.Query))
	if query != "" && !strings.Contains(line, query) {
		return false
	}
	node := strings.TrimSpace(strings.ToLower(f.Node))
	if node != "" && !strings.Contains(line, node) {
		return false
	}
	return true
}

func filterLogEntries(entries []LogEntry, filter LogFilter) []LogEntry {
	if filter.empty() {
		return entries
	}
	filtered := make([]LogEntry, 0, len(entries))
	for _, entry := range entries {
		if filter.match(entry) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func New(level string, accessLog bool) *Logger {
	l := &Logger{buffer: newLogBuffer(DefaultBufferCapacity)}
	l.Configure(level, accessLog)
	return l
}

// Configure updates logging behavior for future writes.
func (l *Logger) Configure(level string, accessLog bool) {
	l.level.Store(int64(levels[normalizeLevel(level)]))
	l.accessLog.Store(accessLog)
}

func (l *Logger) NextRequestID(prefix string) string {
	if prefix == "" {
		prefix = "req"
	}
	n := l.seq.Add(1)
	return fmt.Sprintf("%s-%s-%x", prefix, strconv36(time.Now().UnixMilli()), n)
}

func (l *Logger) AccessEnabled() bool {
	return l.accessLog.Load()
}

func (l *Logger) Enabled(level string) bool {
	return int(l.level.Load()) >= levels[normalizeLevel(level)]
}

func (l *Logger) Subscribe(buf int) (<-chan LogEntry, func()) {
	if l == nil {
		ch := make(chan LogEntry)
		close(ch)
		return ch, func() {}
	}
	if buf < 1 {
		buf = 1
	}
	ch := make(chan LogEntry, buf)
	l.subMu.Lock()
	if l.subs == nil {
		l.subs = make(map[chan LogEntry]struct{})
	}
	l.subs[ch] = struct{}{}
	l.subMu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			l.subMu.Lock()
			if _, ok := l.subs[ch]; ok {
				delete(l.subs, ch)
				close(ch)
			}
			l.subMu.Unlock()
		})
	}
	return ch, cancel
}

// ObserveErrors 注册一个 ERROR 日志观察者，返回取消函数。观察者在写日志的 goroutine 里
// 同步调用（此时不持有日志锁），必须立刻返回，通常只是把记录投进带缓冲的 channel。
// 与日志等级无关：即使控制台等级是 silent，ERROR 也会交给观察者。
func (l *Logger) ObserveErrors(fn func(ErrorRecord)) func() {
	if l == nil || fn == nil {
		return func() {}
	}
	l.obsMu.Lock()
	if l.errObs == nil {
		l.errObs = map[uint64]func(ErrorRecord){}
	}
	l.obsSeq++
	id := l.obsSeq
	l.errObs[id] = fn
	l.obsMu.Unlock()
	return func() {
		l.obsMu.Lock()
		delete(l.errObs, id)
		l.obsMu.Unlock()
	}
}

func (l *Logger) notifyErrorObservers(scope, msg string, meta map[string]any) {
	l.obsMu.RLock()
	observers := make([]func(ErrorRecord), 0, len(l.errObs))
	for _, fn := range l.errObs {
		observers = append(observers, fn)
	}
	l.obsMu.RUnlock()
	if len(observers) == 0 {
		return
	}
	rec := ErrorRecord{
		Time:    time.Now(),
		Scope:   scope,
		Message: RedactText(msg),
		Event:   promotedMetaValue(meta, "event"),
	}
	if value, ok := meta["error"]; ok && value != nil {
		rec.Error = RedactText(fmt.Sprint(value))
	}
	// 锁外回调：观察者里再写 ERROR 或取消订阅都不会卡死。
	for _, fn := range observers {
		fn(rec)
	}
}

func (l *Logger) broadcast(entry LogEntry) {
	if l == nil {
		return
	}
	l.subMu.Lock()
	defer l.subMu.Unlock()
	for ch := range l.subs {
		if entry.Type != "" {
			broadcastControlEntry(ch, entry)
			continue
		}
		select {
		case ch <- entry:
		default:
		}
	}
}

func broadcastControlEntry(ch chan LogEntry, entry LogEntry) {
	for {
		select {
		case ch <- entry:
			return
		default:
		}
		// Control events mark stream boundaries, so drop stale buffered log lines
		// instead of losing the boundary or blocking on a slow SSE client.
		select {
		case <-ch:
		default:
		}
	}
}

func (l *Logger) Entries(limit int) []LogEntry {
	if l == nil || l.buffer == nil {
		return nil
	}
	return l.buffer.Entries(limit)
}

// Clear removes buffered and persisted console log entries.
func (l *Logger) Clear() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.history != nil {
		if err := l.history.reset(); err != nil {
			return err
		}
	}
	if l.buffer != nil {
		l.buffer.Clear()
	}
	l.broadcast(LogEntry{Type: "clear"})
	return nil
}

func (l *Logger) BufferCapacity() int {
	if l == nil || l.buffer == nil {
		return 0
	}
	return l.buffer.Capacity()
}

func (l *Logger) NewestID() uint64 {
	if l == nil || l.buffer == nil {
		return 0
	}
	return l.buffer.NewestID()
}

func (l *Logger) EnableHistory(path string, entriesPerFile, maxFiles int) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.history != nil {
		if err := l.history.Close(); err != nil {
			return err
		}
		l.history = nil
	}
	history, err := newLogHistory(path, entriesPerFile, maxFiles)
	if err != nil {
		return err
	}
	history.onFlushError = func(err error) {
		l.reportHistoryFailure("consoleLogHistoryFlushFailed", "console log history flush failed", err)
	}
	l.history = history
	return nil
}

// ReconfigureHistory 在不重启、不丢弃已有日志的前提下调整落盘保留量。
// 未启用落盘历史时是空操作。
func (l *Logger) ReconfigureHistory(entriesPerFile, maxFiles int) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.history == nil {
		return nil
	}
	return l.history.Reconfigure(entriesPerFile, maxFiles)
}

// HistorySettings 返回当前生效的落盘保留量；ok 为 false 表示未启用落盘历史。
func (l *Logger) HistorySettings() (entriesPerFile int, maxFiles int, ok bool) {
	if l == nil {
		return 0, 0, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.history == nil {
		return 0, 0, false
	}
	return l.history.Settings()
}

func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.history == nil {
		return nil
	}
	return l.history.Close()
}

func (l *Logger) Page(limit int, before uint64, filter LogFilter) LogPage {
	if l == nil {
		return LogPage{}
	}
	if l.buffer != nil {
		entries, hasOlder := l.buffer.EntriesBefore(limit, before, filter)
		if l.history != nil && !filter.empty() {
			historyEntries, historyHasOlder, err := l.history.Page(limit, before, filter)
			if err == nil {
				var truncated bool
				entries, truncated = mergeLogEntriesForPage(limit, historyEntries, entries)
				return LogPage{Entries: entries, HasOlder: hasOlder || historyHasOlder || truncated, History: true}
			}
		}
		if before == 0 || len(entries) > 0 {
			if l.history != nil {
				if len(entries) > 0 && l.history.HasBefore(entries[0].ID) {
					hasOlder = true
				}
				return LogPage{Entries: entries, HasOlder: hasOlder, History: true}
			}
			return LogPage{Entries: entries, HasOlder: hasOlder}
		}
	}
	if l.history != nil {
		entries, hasOlder, err := l.history.Page(limit, before, filter)
		if err == nil {
			return LogPage{Entries: entries, HasOlder: hasOlder, History: true}
		}
	}
	if l.buffer == nil {
		return LogPage{}
	}
	entries, hasOlder := l.buffer.EntriesBefore(limit, before, filter)
	return LogPage{Entries: entries, HasOlder: hasOlder}
}

func mergeLogEntriesForPage(limit int, pages ...[]LogEntry) ([]LogEntry, bool) {
	seen := map[uint64]bool{}
	entries := []LogEntry{}
	for _, page := range pages {
		for _, entry := range page {
			if entry.ID > 0 {
				if seen[entry.ID] {
					continue
				}
				seen[entry.ID] = true
			}
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	if limit <= 0 || limit >= len(entries) {
		return entries, false
	}
	return entries[len(entries)-limit:], true
}

func (l *Logger) PageNumber(limit, page int, filter LogFilter) LogPage {
	if l == nil {
		return LogPage{}
	}
	if l.history != nil {
		entries, totalEntries, totalPages, page, hasOlder, err := l.history.PageNumber(limit, page, filter)
		if err == nil {
			return LogPage{Entries: entries, HasOlder: hasOlder, History: true, Page: page, TotalPages: totalPages, TotalEntries: totalEntries}
		}
	}
	if l.buffer == nil {
		return LogPage{}
	}
	entries, totalEntries, totalPages, page, hasOlder := l.buffer.PageNumber(limit, page, filter)
	return LogPage{Entries: entries, HasOlder: hasOlder, Page: page, TotalPages: totalPages, TotalEntries: totalEntries}
}

func (l *Logger) Debug(scope, msg string, meta map[string]any) { l.write("debug", scope, msg, meta) }
func (l *Logger) Info(scope, msg string, meta map[string]any)  { l.write("info", scope, msg, meta) }
func (l *Logger) Warn(scope, msg string, meta map[string]any)  { l.write("warn", scope, msg, meta) }
func (l *Logger) Error(scope, msg string, meta map[string]any) { l.write("error", scope, msg, meta) }

// ErrorThrottled 与 Error 相同，但同一 scope+event 在 interval 内只真正写一条日志，
// 期间压下的条数记在下一条的 suppressed 字段里。用于持久性故障下每个请求都会失败的位置
// （数据库写入、文件写入等），免得刷屏。每次调用仍会交给 ERROR 观察者，告警里的条数不受影响。
func (l *Logger) ErrorThrottled(interval time.Duration, scope, msg string, meta map[string]any) {
	if l == nil {
		return
	}
	key := scope + "|" + promotedMetaValue(meta, "event")
	if key == scope+"|" {
		key += msg
	}
	allowed, suppressed := l.throttleAllow(key, interval)
	if !allowed {
		l.notifyErrorObservers(scope, msg, meta)
		return
	}
	if suppressed > 0 {
		merged := make(map[string]any, len(meta)+1)
		for k, v := range meta {
			merged[k] = v
		}
		merged["suppressed"] = suppressed
		meta = merged
	}
	l.write("error", scope, msg, meta)
}

func (l *Logger) throttleAllow(key string, interval time.Duration) (bool, int) {
	now := time.Now()
	l.thrMu.Lock()
	defer l.thrMu.Unlock()
	if l.thr == nil {
		l.thr = map[string]*throttleState{}
	}
	st := l.thr[key]
	if st == nil {
		st = &throttleState{}
		l.thr[key] = st
	}
	if !st.last.IsZero() && now.Sub(st.last) < interval {
		st.suppressed++
		return false, 0
	}
	suppressed := st.suppressed
	st.last, st.suppressed = now, 0
	return true, suppressed
}

// reportHistoryFailure 把落盘失败报成节流后的 ERROR。这条报错本身不再写落盘历史，
// 否则它的写入失败又会触发一次上报。
func (l *Logger) reportHistoryFailure(event, msg string, err error) {
	meta := map[string]any{"event": event, "error": err.Error()}
	allowed, suppressed := l.throttleAllow("logging|"+event, historyFailureInterval)
	if !allowed {
		l.notifyErrorObservers("logging", msg, meta)
		return
	}
	if suppressed > 0 {
		meta["suppressed"] = suppressed
	}
	l.writeEntry("error", "logging", msg, meta, false)
}

func (l *Logger) write(level, scope, msg string, meta map[string]any) {
	l.writeEntry(level, scope, msg, meta, true)
}

func (l *Logger) writeEntry(level, scope, msg string, meta map[string]any, persist bool) {
	level = normalizeLevel(level)
	status := promotedMetaValue(meta, "status")
	parts := []string{localtime.RFC3339(time.Now()), "[" + strings.ToUpper(level) + "]"}
	if status != "" {
		parts = append(parts, "["+status+"]")
	}
	parts = append(parts, "["+scope+"]")
	if clean := RedactText(msg); clean != "" && promotedMetaValue(meta, "event") == "" {
		parts = append(parts, clean)
	}
	if formatted := formatMeta(meta); formatted != "" {
		parts = append(parts, formatted)
	}
	line := strings.Join(parts, " ")
	entry := LogEntry{Time: parts[0], Level: level, Scope: scope, Message: RedactText(msg), Line: line}
	var historyErr error
	l.mu.Lock()
	if l.buffer != nil {
		entry = l.buffer.Append(entry)
	}
	if l.history != nil && persist {
		historyErr = l.history.Append(entry)
	}
	l.broadcast(entry)
	l.mu.Unlock()
	if level == "error" {
		l.notifyErrorObservers(scope, msg, meta)
	}
	if historyErr != nil {
		var rotateErr historyRotateError
		if errors.As(historyErr, &rotateErr) {
			l.reportHistoryFailure("consoleLogHistoryRotateFailed", "console log history rotate failed", rotateErr.err)
		} else {
			l.reportHistoryFailure("consoleLogHistoryWriteFailed", "console log history write failed", historyErr)
		}
	}
	if !l.Enabled(level) {
		return
	}
	if level == "error" || level == "warn" {
		fmt.Fprintln(os.Stderr, line)
		return
	}
	fmt.Fprintln(os.Stdout, line)
}

func newLogBuffer(capacity int) *logBuffer {
	if capacity < 1 {
		capacity = 1
	}
	return &logBuffer{capacity: capacity, entries: make([]LogEntry, 0, capacity)}
}

func (b *logBuffer) Append(entry LogEntry) LogEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	entry.ID = b.next
	if len(b.entries) < b.capacity {
		b.entries = append(b.entries, entry)
		return entry
	}
	b.entries[b.start] = entry
	b.start = (b.start + 1) % b.capacity
	return entry
}

func (b *logBuffer) Entries(limit int) []LogEntry {
	entries, _ := b.EntriesBefore(limit, 0, LogFilter{})
	return entries
}

func (b *logBuffer) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.start = 0
	b.entries = b.entries[:0]
}

func (b *logBuffer) EntriesBefore(limit int, before uint64, filter LogFilter) ([]LogEntry, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	total := len(b.entries)
	if limit <= 0 || limit > total {
		limit = total
	}
	all := make([]LogEntry, 0, total)
	for i := 0; i < total; i++ {
		idx := (b.start + i) % b.capacity
		entry := b.entries[idx]
		if (before == 0 || entry.ID < before) && (filter.empty() || filter.match(entry)) {
			all = append(all, entry)
		}
	}
	if limit <= 0 || limit > len(all) {
		limit = len(all)
	}
	hasOlder := len(all) > limit
	if hasOlder {
		all = all[len(all)-limit:]
	}
	return all, hasOlder
}

func (b *logBuffer) PageNumber(limit, page int, filter LogFilter) ([]LogEntry, int, int, int, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	limit = normalizePageLimit(limit, b.capacity)
	entries := make([]LogEntry, 0, len(b.entries))
	for i := 0; i < len(b.entries); i++ {
		idx := (b.start + i) % b.capacity
		entry := b.entries[idx]
		if filter.empty() || filter.match(entry) {
			entries = append(entries, entry)
		}
	}
	total := len(entries)
	totalPages := logPageCount(total, limit)
	page = clampLogPage(page, totalPages)
	start, end := logPageBounds(total, limit, page)
	out := append([]LogEntry(nil), entries[start:end]...)
	return out, total, totalPages, page, start > 0
}

func (b *logBuffer) Capacity() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.capacity
}

func (b *logBuffer) NewestID() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.entries) == 0 {
		return 0
	}
	idx := (b.start + len(b.entries) - 1) % b.capacity
	return b.entries[idx].ID
}

func newLogHistory(path string, entriesPerFile, maxFiles int) (*logHistory, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("log history path is empty")
	}
	if entriesPerFile < 1 {
		entriesPerFile = DefaultHistoryEntriesFile
	}
	if maxFiles < 1 {
		maxFiles = DefaultHistoryRotatedFiles
	}
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return nil, err
	}
	h := &logHistory{path: path, entriesPerFile: entriesPerFile, maxFiles: maxFiles, done: make(chan struct{})}
	if err := h.reset(); err != nil {
		return nil, err
	}
	go h.flushLoop()
	return h, nil
}

func (h *logHistory) reset() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.closeWriterLocked(); err != nil {
		return err
	}
	if err := h.removeRotatedLocked(0); err != nil {
		return err
	}
	h.entryCount = 0
	h.retainedEntries = 0
	h.oldestID = 0
	return nil
}

// Reconfigure 调整保留量并清掉超出新保留范围的轮转文件；当前正在写的文件句柄保持可用。
func (h *logHistory) Reconfigure(entriesPerFile, maxFiles int) error {
	if h == nil {
		return nil
	}
	if entriesPerFile < 1 {
		entriesPerFile = DefaultHistoryEntriesFile
	}
	if maxFiles < 1 {
		maxFiles = DefaultHistoryRotatedFiles
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	if h.entriesPerFile == entriesPerFile && h.maxFiles == maxFiles {
		return nil
	}
	// 先落盘，避免删轮转文件时丢掉缓冲区里的内容。
	if err := h.flushWriterLocked(); err != nil {
		return err
	}
	if err := h.removeRotatedLocked(maxFiles); err != nil {
		return err
	}
	h.entriesPerFile = entriesPerFile
	h.maxFiles = maxFiles
	// 缩小保留量后，估算值可能超过新容量；沿用 rotateLocked 的近似口径修正。
	if capacity := h.entriesPerFile * h.maxFiles; capacity > 0 && h.retainedEntries > capacity {
		h.oldestID += uint64(h.retainedEntries - capacity)
		h.retainedEntries = capacity
	}
	// 当前文件已经写满新的单文件条数时，下一条 Append 会自然触发轮转。
	return nil
}

func (h *logHistory) Settings() (int, int, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.entriesPerFile, h.maxFiles, true
}

// removeRotatedLocked 删除索引 >= keep 的轮转文件；keep 为 0 时连当前文件一起删。
func (h *logHistory) removeRotatedLocked(keep int) error {
	if keep <= 0 {
		if err := os.Remove(h.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		keep = 1
	}
	sweep := logHistorySweepFiles
	if h.maxFiles > sweep {
		sweep = h.maxFiles
	}
	for i := keep; i <= sweep; i++ {
		if err := os.Remove(rotatedLogPath(h.path, i)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (h *logHistory) Append(entry LogEntry) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	if h.entryCount >= h.entriesPerFile {
		if err := h.rotateLocked(); err != nil {
			return historyRotateError{err: err}
		}
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if err := h.ensureWriterLocked(); err != nil {
		return err
	}
	if _, err := h.writer.Write(b); err != nil {
		h.discardWriterLocked()
		return err
	}
	if err := h.writer.WriteByte('\n'); err != nil {
		h.discardWriterLocked()
		return err
	}
	h.entryCount++
	h.retainedEntries++
	if h.oldestID == 0 || h.retainedEntries == 1 {
		h.oldestID = entry.ID
	}
	return nil
}

func (h *logHistory) Close() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	close(h.done)
	return h.closeWriterLocked()
}

func (h *logHistory) flushLoop() {
	ticker := time.NewTicker(logHistoryFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			h.mu.Lock()
			if h.closed {
				h.mu.Unlock()
				return
			}
			err := h.flushWriterLocked()
			if err != nil {
				h.discardWriterLocked()
			}
			onError := h.onFlushError
			h.mu.Unlock()
			if err != nil && onError != nil {
				onError(err)
			}
		case <-h.done:
			return
		}
	}
}

func (h *logHistory) ensureWriterLocked() error {
	if h.writer != nil && h.file != nil {
		return nil
	}
	f, err := os.OpenFile(h.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	h.file = f
	h.writer = bufio.NewWriterSize(f, logHistoryBufferSize)
	return nil
}

func (h *logHistory) flushWriterLocked() error {
	if h.writer == nil {
		return nil
	}
	if err := h.writer.Flush(); err != nil {
		return err
	}
	return nil
}

// discardWriterLocked 丢掉出过错的写入器。bufio.Writer 一旦写失败就永久处于错误状态，
// 不丢掉的话磁盘恢复后也写不进去；缓冲里的内容此时已经写不出去了。下一次 Append 会重新打开文件。
func (h *logHistory) discardWriterLocked() {
	h.writer = nil
	if h.file != nil {
		_ = h.file.Close()
		h.file = nil
	}
}

func (h *logHistory) closeWriterLocked() error {
	var firstErr error
	if h.writer != nil {
		if err := h.writer.Flush(); err != nil {
			firstErr = err
		}
		h.writer = nil
	}
	if h.file != nil {
		if err := h.file.Close(); firstErr == nil && err != nil {
			firstErr = err
		}
		h.file = nil
	}
	return firstErr
}

func (h *logHistory) HasBefore(id uint64) bool {
	if h == nil || id == 0 {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.retainedEntries > 0 && h.oldestID > 0 && h.oldestID < id
}

func (h *logHistory) Page(limit int, before uint64, filter LogFilter) ([]LogEntry, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.flushWriterLocked(); err != nil {
		return nil, false, err
	}
	if limit <= 0 {
		limit = h.entriesPerFile
	}
	entries := []LogEntry{}
	for _, path := range h.pathsOldestFirst() {
		if err := readLogEntries(path, before, &entries); err != nil {
			return nil, false, err
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	entries = filterLogEntries(entries, filter)
	if limit > len(entries) {
		limit = len(entries)
	}
	hasOlder := len(entries) > limit
	if hasOlder {
		entries = entries[len(entries)-limit:]
	}
	return entries, hasOlder, nil
}

func (h *logHistory) PageNumber(limit, page int, filter LogFilter) ([]LogEntry, int, int, int, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.flushWriterLocked(); err != nil {
		return nil, 0, 1, 1, false, err
	}
	limit = normalizePageLimit(limit, h.entriesPerFile)
	entries := []LogEntry{}
	for _, path := range h.pathsOldestFirst() {
		if err := readLogEntries(path, 0, &entries); err != nil {
			return nil, 0, 1, 1, false, err
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	entries = filterLogEntries(entries, filter)
	total := len(entries)
	totalPages := logPageCount(total, limit)
	page = clampLogPage(page, totalPages)
	start, end := logPageBounds(total, limit, page)
	out := append([]LogEntry(nil), entries[start:end]...)
	return out, total, totalPages, page, start > 0, nil
}

func (h *logHistory) rotateLocked() error {
	if err := h.closeWriterLocked(); err != nil {
		return err
	}
	if h.maxFiles <= 1 {
		if err := os.Remove(h.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		h.entryCount = 0
		h.retainedEntries = 0
		h.oldestID = 0
		return nil
	}
	oldest := rotatedLogPath(h.path, h.maxFiles-1)
	if err := os.Remove(oldest); err != nil && !os.IsNotExist(err) {
		return err
	}
	if capacity := h.entriesPerFile * h.maxFiles; h.retainedEntries >= capacity {
		h.retainedEntries -= h.entriesPerFile
		h.oldestID += uint64(h.entriesPerFile)
	}
	for i := h.maxFiles - 2; i >= 1; i-- {
		from := rotatedLogPath(h.path, i)
		to := rotatedLogPath(h.path, i+1)
		if err := os.Rename(from, to); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Rename(h.path, rotatedLogPath(h.path, 1)); err != nil && !os.IsNotExist(err) {
		return err
	}
	h.entryCount = 0
	return nil
}

func (h *logHistory) pathsOldestFirst() []string {
	paths := make([]string, 0, h.maxFiles)
	for i := h.maxFiles - 1; i >= 1; i-- {
		paths = append(paths, rotatedLogPath(h.path, i))
	}
	paths = append(paths, h.path)
	return paths
}

func rotatedLogPath(path string, index int) string {
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	return base + "." + strconv.Itoa(index) + ext
}

func readLogEntries(path string, before uint64, out *[]LogEntry) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var entry LogEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.ID == 0 || (before > 0 && entry.ID >= before) {
			continue
		}
		*out = append(*out, entry)
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return err
	}
	return nil
}

func normalizePageLimit(limit, fallback int) int {
	if limit > 0 {
		return limit
	}
	if fallback > 0 {
		return fallback
	}
	return 1
}

func logPageCount(total, limit int) int {
	if total <= 0 {
		return 1
	}
	if limit <= 0 {
		return 1
	}
	return (total + limit - 1) / limit
}

func clampLogPage(page, totalPages int) int {
	if totalPages < 1 {
		totalPages = 1
	}
	if page < 1 {
		return 1
	}
	if page > totalPages {
		return totalPages
	}
	return page
}

func logPageBounds(total, limit, page int) (int, int) {
	if total <= 0 {
		return 0, 0
	}
	end := total - (page-1)*limit
	if end < 0 {
		end = 0
	}
	if end > total {
		end = total
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	return start, end
}

func RedactURL(raw string) string {
	value := cleanString(raw, 512)
	if value == "" {
		return ""
	}
	hasOrigin := httpURLRE.MatchString(value)
	u, err := url.Parse(value)
	if err == nil {
		if !hasOrigin {
			u, err = url.Parse("http://local" + ensureLeadingSlash(value))
		}
		if err == nil {
			out := u.EscapedPath()
			if out == "" {
				out = "/"
			}
			if hasOrigin {
				return u.Scheme + "://" + u.Host + out
			}
			return out
		}
	}
	idx := strings.IndexByte(value, '#')
	if idx >= 0 {
		value = value[:idx]
	}
	q := strings.IndexByte(value, '?')
	if q < 0 {
		return value
	}
	return value[:q]
}

func RedactText(raw string) string {
	value := cleanString(raw, 512)
	if value == "" {
		return ""
	}
	return embeddedHTTPURLRE.ReplaceAllStringFunc(value, RedactURL)
}

func RedactProxyURL(raw, nodeName, secret string) string {
	redacted := RedactURL(raw)
	node := strings.ToLower(strings.TrimSpace(nodeName))
	secret = strings.TrimSpace(secret)
	if node == "" || secret == "" || redacted == "" {
		return redacted
	}
	variants := []string{secret, url.PathEscape(secret)}
	for _, v := range variants {
		marker := "/" + node + "/" + v
		if strings.HasPrefix(strings.ToLower(redacted), strings.ToLower(marker)) {
			return "/" + node + "/<secret>" + redacted[len(marker):]
		}
	}
	return redacted
}

func FormatTarget(target string) string {
	u, err := url.Parse(target)
	if err == nil && u.Scheme != "" && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return RedactURL(target)
}

func ensureLeadingSlash(value string) string {
	if strings.HasPrefix(value, "/") {
		return value
	}
	return "/" + value
}

func cleanString(value string, maxLen int) string {
	replacer := strings.NewReplacer("\r", " ", "\n", " ", "\t", " ")
	s := strings.TrimSpace(replacer.Replace(value))
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}

func formatMeta(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	keys := make([]string, 0, len(meta))
	for key, value := range meta {
		if key == "status" {
			continue
		}
		if value != nil && fmt.Sprint(value) != "" {
			keys = append(keys, key)
		}
	}
	sortMetaKeys(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+formatValue(meta[key]))
	}
	return strings.Join(parts, " ")
}

func sortMetaKeys(keys []string) {
	sort.Slice(keys, func(i, j int) bool {
		left, leftOK := metaFieldOrder[keys[i]]
		right, rightOK := metaFieldOrder[keys[j]]
		if leftOK || rightOK {
			if !leftOK {
				return false
			}
			if !rightOK {
				return true
			}
			if left != right {
				return left < right
			}
		}
		return keys[i] < keys[j]
	})
}

func promotedMetaValue(meta map[string]any, key string) string {
	if len(meta) == 0 {
		return ""
	}
	value, ok := meta[key]
	if !ok || value == nil || fmt.Sprint(value) == "" {
		return ""
	}
	return formatValue(value)
}

func formatValue(value any) string {
	s := RedactText(fmt.Sprint(value))
	if s == "" {
		return ""
	}
	if safeLogValueRE.MatchString(s) {
		return s
	}
	return fmt.Sprintf("%q", s)
}

func normalizeLevel(value string) string {
	v := strings.ToLower(strings.TrimSpace(value))
	if _, ok := levels[v]; ok {
		return v
	}
	return "info"
}

func strconv36(n int64) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	if n == 0 {
		return "0"
	}
	var b [32]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = digits[n%36]
		n /= 36
	}
	return string(b[i:])
}
