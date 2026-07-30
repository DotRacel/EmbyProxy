// Package probe 周期性探测各 Emby 节点的上游线路，把延迟与可达性样本保存在内存里，
// 供管理面板绘制延迟曲线、迷你走势图和 24 小时可用率。样本不落库，重启后从零开始。
package probe

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"embyproxy/internal/logging"
	"embyproxy/internal/storage"
)

const (
	// Interval 后台探测周期。24 小时可用率与延迟曲线都依赖这个采样节奏。
	Interval = time.Minute
	// Retention 样本保留时长，与面板上的 24 小时可用率对齐。
	Retention = 24 * time.Hour
	// ProbeTimeout 单条上游线路的探测超时。
	ProbeTimeout = 8 * time.Second
	// SparkPoints 列表卡片迷你走势图的点数。
	SparkPoints = 12

	probePath   = "/emby/System/Info/Public"
	probeAgent  = "emby-proxy-probe/1.0"
	maxSamples  = int(Retention/Interval) + 16
	targetLimit = 8
)

// Sample 一次探测结果。失败时仍记录耗时，用于区分超时与秒拒。
type Sample struct {
	At     int64  `json:"at"`
	MS     int64  `json:"ms"`
	Status int    `json:"status"`
	OK     bool   `json:"ok"`
	Err    string `json:"err,omitempty"`
}

// TargetStats 单条上游线路的最新探测结果。
type TargetStats struct {
	Target  string `json:"target"`
	At      int64  `json:"at,omitempty"`
	MS      int64  `json:"ms"`
	Status  int    `json:"status"`
	OK      bool   `json:"ok"`
	Err     string `json:"err,omitempty"`
	Primary bool   `json:"primary"`
	Samples int    `json:"samples"`
}

// NodeStats 节点的探测概况，对应设计稿里列表卡片与详情页头部的指标。
type NodeStats struct {
	Name         string        `json:"name"`
	LastAt       int64         `json:"lastAt,omitempty"`
	LastMS       int64         `json:"lastMs"`
	LastStatus   int           `json:"lastStatus"`
	LastError    string        `json:"lastError,omitempty"`
	OK           bool          `json:"ok"`
	Probed       bool          `json:"probed"`
	Samples      int           `json:"samples"`
	Availability float64       `json:"availability"`
	AvgMS        int64         `json:"avgMs"`
	MaxMS        int64         `json:"maxMs"`
	Spark        []int64       `json:"spark"`
	Targets      []TargetStats `json:"targets"`
}

// Point 延迟曲线上的一个时间桶。MS 为 -1 表示该区间没有成功样本。
type Point struct {
	At int64   `json:"at"`
	MS int64   `json:"ms"`
	OK float64 `json:"ok"`
}

type series struct {
	samples []Sample
}

func (s *series) push(v Sample) {
	s.samples = append(s.samples, v)
	cutoff := v.At - Retention.Milliseconds()
	drop := 0
	for drop < len(s.samples) && s.samples[drop].At < cutoff {
		drop++
	}
	if len(s.samples)-drop > maxSamples {
		drop = len(s.samples) - maxSamples
	}
	if drop > 0 {
		s.samples = append(s.samples[:0], s.samples[drop:]...)
	}
}

func (s *series) window(since int64) []Sample {
	if s == nil {
		return nil
	}
	idx := sort.Search(len(s.samples), func(i int) bool { return s.samples[i].At >= since })
	return s.samples[idx:]
}

// Registry 保存所有节点与上游线路的探测样本。所有方法都可并发调用。
type Registry struct {
	mu      sync.RWMutex
	nodes   map[string]*series
	targets map[string]map[string]*series
}

func NewRegistry() *Registry {
	return &Registry{
		nodes:   map[string]*series{},
		targets: map[string]map[string]*series{},
	}
}

func (r *Registry) RecordNode(node string, v Sample) {
	if r == nil || node == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.nodes[node]
	if s == nil {
		s = &series{}
		r.nodes[node] = s
	}
	s.push(v)
}

func (r *Registry) RecordTarget(node, target string, v Sample) {
	if r == nil || node == "" || target == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	byTarget := r.targets[node]
	if byTarget == nil {
		byTarget = map[string]*series{}
		r.targets[node] = byTarget
	}
	s := byTarget[target]
	if s == nil {
		s = &series{}
		byTarget[target] = s
	}
	s.push(v)
}

// Retain 丢弃已经不存在的节点的样本，避免删除或改名后一直占着内存。
func (r *Registry) Retain(names []string) {
	if r == nil {
		return
	}
	keep := make(map[string]bool, len(names))
	for _, name := range names {
		keep[name] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for name := range r.nodes {
		if !keep[name] {
			delete(r.nodes, name)
		}
	}
	for name := range r.targets {
		if !keep[name] {
			delete(r.targets, name)
		}
	}
}

// Forget 删除单个节点的样本。
func (r *Registry) Forget(name string) {
	if r == nil || name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.nodes, name)
	delete(r.targets, name)
}

// Stats 汇总一个节点的探测概况。targets 按上游线路的配置顺序给出，第一条为主线。
func (r *Registry) Stats(name string, targets []string) NodeStats {
	out := NodeStats{Name: name, LastMS: -1, Spark: []int64{}, Targets: []TargetStats{}}
	if r == nil {
		return out
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	samples := r.nodes[name].window(time.Now().Add(-Retention).UnixMilli())
	out.Samples = len(samples)
	if len(samples) > 0 {
		last := samples[len(samples)-1]
		out.Probed = true
		out.LastAt = last.At
		out.LastMS = last.MS
		out.LastStatus = last.Status
		out.LastError = last.Err
		out.OK = last.OK

		okCount, sum, okSamples := 0, int64(0), 0
		for _, s := range samples {
			if !s.OK {
				continue
			}
			okCount++
			sum += s.MS
			okSamples++
			if s.MS > out.MaxMS {
				out.MaxMS = s.MS
			}
		}
		out.Availability = float64(okCount) / float64(len(samples))
		if okSamples > 0 {
			out.AvgMS = sum / int64(okSamples)
		}
		out.Spark = spark(samples, SparkPoints)
	}

	byTarget := r.targets[name]
	for i, target := range targets {
		if i >= targetLimit {
			break
		}
		stat := TargetStats{Target: target, MS: -1, Primary: i == 0}
		if s := byTarget[target]; s != nil && len(s.samples) > 0 {
			last := s.samples[len(s.samples)-1]
			stat.At = last.At
			stat.MS = last.MS
			stat.Status = last.Status
			stat.OK = last.OK
			stat.Err = last.Err
			stat.Samples = len(s.samples)
		}
		out.Targets = append(out.Targets, stat)
	}
	return out
}

// Series 把最近 window 时长内的样本压成 buckets 个等宽时间桶，供前端画延迟曲线。
func (r *Registry) Series(name string, window time.Duration, buckets int) []Point {
	if r == nil || buckets <= 0 || window <= 0 {
		return []Point{}
	}
	if window > Retention {
		window = Retention
	}
	now := time.Now().UnixMilli()
	since := now - window.Milliseconds()
	span := window.Milliseconds() / int64(buckets)
	if span <= 0 {
		span = 1
	}

	r.mu.RLock()
	samples := append([]Sample(nil), r.nodes[name].window(since)...)
	r.mu.RUnlock()

	sums := make([]int64, buckets)
	okCounts := make([]int, buckets)
	totals := make([]int, buckets)
	for _, s := range samples {
		idx := int((s.At - since) / span)
		if idx < 0 {
			idx = 0
		}
		if idx >= buckets {
			idx = buckets - 1
		}
		totals[idx]++
		if s.OK {
			okCounts[idx]++
			sums[idx] += s.MS
		}
	}

	points := make([]Point, 0, buckets)
	for i := 0; i < buckets; i++ {
		p := Point{At: since + int64(i)*span, MS: -1}
		if okCounts[i] > 0 {
			p.MS = sums[i] / int64(okCounts[i])
		}
		if totals[i] > 0 {
			p.OK = float64(okCounts[i]) / float64(totals[i])
		}
		points = append(points, p)
	}
	return points
}

// spark 把样本等分降采样成 points 个点，失败区间记 -1。
func spark(samples []Sample, points int) []int64 {
	if len(samples) == 0 || points <= 0 {
		return []int64{}
	}
	if len(samples) <= points {
		out := make([]int64, 0, len(samples))
		for _, s := range samples {
			if s.OK {
				out = append(out, s.MS)
			} else {
				out = append(out, -1)
			}
		}
		return out
	}
	out := make([]int64, 0, points)
	size := float64(len(samples)) / float64(points)
	for i := 0; i < points; i++ {
		start := int(float64(i) * size)
		end := int(float64(i+1) * size)
		if end > len(samples) {
			end = len(samples)
		}
		if end <= start {
			end = start + 1
		}
		sum, count := int64(0), 0
		for _, s := range samples[start:end] {
			if s.OK {
				sum += s.MS
				count++
			}
		}
		if count == 0 {
			out = append(out, -1)
			continue
		}
		out = append(out, sum/int64(count))
	}
	return out
}

// Prober 按 Interval 周期探测所有节点的上游线路。
type Prober struct {
	registry *Registry
	store    *storage.Store
	log      *logging.Logger
	client   *http.Client
}

func NewProber(registry *Registry, store *storage.Store, log *logging.Logger) *Prober {
	return &Prober{
		registry: registry,
		store:    store,
		log:      log,
		client:   &http.Client{Timeout: ProbeTimeout},
	}
}

func (p *Prober) Start(ctx context.Context) {
	if p == nil || p.registry == nil || p.store == nil {
		return
	}
	go func() {
		p.ProbeAll(ctx)
		ticker := time.NewTicker(Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.ProbeAll(ctx)
			}
		}
	}()
}

// ProbeAll 探测全部节点，并清掉已删除节点的历史样本。
func (p *Prober) ProbeAll(ctx context.Context) {
	if p == nil || p.store == nil {
		return
	}
	nodes, err := p.store.ListNodes(ctx, "admin")
	if err != nil {
		if p.log != nil {
			p.log.Debug("probe", "list nodes failed", map[string]any{"event": "probeListFailed", "error": err.Error()})
		}
		return
	}
	names := make([]string, 0, len(nodes))
	for _, node := range nodes {
		names = append(names, node.Name)
	}
	p.registry.Retain(names)
	for _, node := range nodes {
		select {
		case <-ctx.Done():
			return
		default:
		}
		p.ProbeNode(ctx, node)
	}
}

// ProbeNode 依次探测节点的每条上游线路，并把「第一条可用线路」记为节点整体状态，
// 与代理实际的故障转移顺序保持一致。
func (p *Prober) ProbeNode(ctx context.Context, node storage.Node) Sample {
	targets := storage.SplitTargets(node.Target)
	if len(targets) > targetLimit {
		targets = targets[:targetLimit]
	}
	var nodeSample Sample
	found := false
	for _, target := range targets {
		sample := p.probeTarget(ctx, target)
		p.registry.RecordTarget(node.Name, target, sample)
		if !found && sample.OK {
			nodeSample = sample
			found = true
		}
		if !found {
			nodeSample = sample
		}
	}
	if len(targets) == 0 {
		nodeSample = Sample{At: time.Now().UnixMilli(), MS: 0, Err: "未配置上游地址"}
	}
	p.registry.RecordNode(node.Name, nodeSample)
	return nodeSample
}

func (p *Prober) probeTarget(ctx context.Context, target string) Sample {
	started := time.Now()
	sample := Sample{At: started.UnixMilli(), MS: 0}

	reqCtx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, strings.TrimRight(target, "/")+probePath, nil)
	if err != nil {
		sample.MS = time.Since(started).Milliseconds()
		sample.Err = err.Error()
		return sample
	}
	req.Header.Set("User-Agent", probeAgent)

	res, err := p.client.Do(req)
	sample.MS = time.Since(started).Milliseconds()
	if err != nil {
		sample.Err = errorText(err)
		return sample
	}
	defer res.Body.Close()
	sample.Status = res.StatusCode
	sample.OK = res.StatusCode >= 200 && res.StatusCode < 400
	if !sample.OK {
		sample.Err = res.Status
	}
	return sample
}

// errorText 把 net/http 的原始错误压成一句能直接显示在面板上的短说明。
// 原始串形如 `Get "http://host/path": dial tcp 1.2.3.4:80: connect: connection refused`，
// 直接塞进上游线路那一行会撑爆版面，也没什么信息量。
func errorText(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	switch {
	case strings.Contains(text, "context deadline exceeded"), strings.Contains(text, "Client.Timeout"):
		return "超时"
	case strings.Contains(text, "connection refused"):
		return "拒绝连接"
	case strings.Contains(text, "no such host"):
		return "域名解析失败"
	case strings.Contains(text, "connection reset"):
		return "连接被重置"
	case strings.Contains(text, "network is unreachable"), strings.Contains(text, "no route to host"):
		return "网络不可达"
	case strings.Contains(text, "certificate"), strings.Contains(text, "tls:"):
		return "证书校验失败"
	case strings.Contains(text, "context canceled"):
		return "已取消"
	}
	// 兜底：去掉 `Get "url":` 前缀，只留最后一段，并限制长度。
	if idx := strings.LastIndex(text, ": "); idx >= 0 && idx+2 < len(text) {
		text = text[idx+2:]
	}
	if len([]rune(text)) > 40 {
		text = string([]rune(text)[:40]) + "…"
	}
	return text
}
