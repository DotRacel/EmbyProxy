package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"embyproxy/internal/config"
	"embyproxy/internal/storage"
)

const (
	imageCacheDirName           = "image-cache"
	imageCacheTTLDaysLimit      = 365
	imageCacheMetaCacheTTL      = time.Minute
	imageCacheMetaCacheMaxItems = 4096
	// 容量淘汰的低水位：超过上限后一直淘汰到上限的 90%，
	// 避免刚好卡在上限上、每写一张图就触发一次淘汰。
	imageCacheEvictLowWaterPct = 90
	// 启动扫描时，body/meta 只剩其一的残缺条目要老于这个时间才删，
	// 免得误删正在写入（body 已 rename、meta 还没落盘）的条目。
	imageCacheOrphanGrace = time.Hour
)

var imageCacheIgnoredQueryParams = map[string]bool{
	"api_key":                       true,
	"authorization":                 true,
	"x-authorization":               true,
	"x-emby-authorization":          true,
	"x-mediabrowser-authorization":  true,
	"x-emby-token":                  true,
	"x-mediabrowser-token":          true,
	"x-emby-client":                 true,
	"x-mediabrowser-client":         true,
	"x-emby-client-version":         true,
	"x-mediabrowser-client-version": true,
	"x-emby-device-id":              true,
	"x-mediabrowser-device-id":      true,
	"x-emby-device-name":            true,
	"x-mediabrowser-device-name":    true,
	"x-emby-language":               true,
	"x-mediabrowser-language":       true,
}

type imageDiskCache struct {
	dir         string
	ttl         time.Duration
	maxBytes    int64
	now         func() time.Time
	cleanupMu   sync.Mutex
	lastCleanup time.Time
	fillMu      sync.Mutex
	fills       map[string]*imageCacheFill
	metaMu      sync.RWMutex
	metaCache   map[string]imageMetaCacheEntry
	// 容量淘汰用的内存索引：hash -> 占用字节 + 最近访问时间。
	// 命中时只改内存里的时间戳，热路径上不做任何额外磁盘 IO。
	indexMu    sync.Mutex
	index      map[string]*imageCacheIndexEntry
	indexBytes int64
	indexReady bool
	indexGen   uint64
	scanOnce   sync.Once
	scanDone   chan struct{}
	evicting   atomic.Bool
	// 命中率统计：纯内存原子计数器，热路径上不加锁、不落盘、不碰数据库。
	// 统计口径是「本缓存实例创建以来」，进程重启、配置变更导致实例重建、
	// 手动清空缓存都会归零，statsSince 记录本轮统计的起点（Unix 秒）。
	hits       atomic.Int64
	misses     atomic.Int64
	statsSince atomic.Int64
}

type imageCacheIndexEntry struct {
	size       int64
	lastAccess int64 // UnixNano
}

type ImageCacheStats struct {
	Enabled    bool    `json:"enabled"`
	Dir        string  `json:"dir"`
	Bytes      int64   `json:"bytes"`
	Files      int     `json:"files"`
	Entries    int     `json:"entries"`
	MaxBytes   int64   `json:"maxBytes"`
	Usage      float64 `json:"usage"`
	IndexReady bool    `json:"indexReady"`
	// Hits/Misses 只统计真正查过缓存的图片请求；Lookups = Hits + Misses，
	// 为 0 表示本轮还没有样本（此时 HitRate 固定为 0，不会是 NaN）。
	// StatsSince 是本轮统计的起始时间（Unix 秒，0 表示没有统计）。
	Hits       int64   `json:"hits"`
	Misses     int64   `json:"misses"`
	Lookups    int64   `json:"lookups"`
	HitRate    float64 `json:"hitRate"`
	StatsSince int64   `json:"statsSince"`
}

type imageCacheMeta struct {
	KeyHash   string      `json:"keyHash"`
	Status    int         `json:"status"`
	Header    http.Header `json:"header"`
	CreatedAt int64       `json:"createdAt"`
	ExpiresAt int64       `json:"expiresAt"`
}

type imageCachePaths struct {
	hash string
	dir  string
	body string
	meta string
}

type imageCacheFill struct {
	hash string
	done chan struct{}
	once sync.Once
}

type imageMetaCacheEntry struct {
	meta imageCacheMeta
	exp  time.Time
}

func newImageCacheFromSystemConfig(cfg config.Config, sys storage.SystemConfig) *imageDiskCache {
	if !sys.ImageCacheEnabled {
		return nil
	}
	cache := newImageDiskCache(imageCacheDir(cfg), imageCacheTTL(sys), sys.ImageCacheMaxBytes())
	cache.startIndexScan()
	return cache
}

func (h *Handler) ensureImageCache(ctx context.Context) *imageDiskCache {
	sys := h.systemConfig(ctx)
	if !sys.ImageCacheEnabled {
		h.imageCacheMu.Lock()
		h.imageCache = nil
		h.imageCacheMu.Unlock()
		return nil
	}
	dir := imageCacheDir(h.cfg)
	ttl := imageCacheTTL(sys)
	maxBytes := sys.ImageCacheMaxBytes()
	h.imageCacheMu.Lock()
	cache := h.imageCache
	if cache == nil || !cache.matches(dir, ttl, maxBytes) {
		cache = newImageDiskCache(dir, ttl, maxBytes)
		h.imageCache = cache
	}
	h.imageCacheMu.Unlock()
	cache.startIndexScan()
	return cache
}

func imageCacheDir(cfg config.Config) string {
	cwd := strings.TrimSpace(cfg.CWD)
	dir := filepath.Join("data", imageCacheDirName)
	if cwd != "" {
		dir = filepath.Join(cwd, "data", imageCacheDirName)
	}
	return dir
}

func imageCacheTTL(sys storage.SystemConfig) time.Duration {
	days := clampImageConfigInt(sys.ImageCacheTTLDays, 1, imageCacheTTLDaysLimit)
	return time.Duration(days) * 24 * time.Hour
}

// newImageDiskCache 创建缓存实例。maxBytes<=0 表示不限制容量，
// 此时索引、扫描、淘汰全部关闭，行为与加容量上限之前完全一致。
func newImageDiskCache(dir string, ttl time.Duration, maxBytes int64) *imageDiskCache {
	dir = strings.TrimSpace(dir)
	if dir == "" || ttl <= 0 {
		return nil
	}
	if maxBytes < 0 {
		maxBytes = 0
	}
	cache := &imageDiskCache{
		dir:      dir,
		ttl:      ttl,
		maxBytes: maxBytes,
		now:      time.Now,
		index:    map[string]*imageCacheIndexEntry{},
		scanDone: make(chan struct{}),
	}
	cache.statsSince.Store(time.Now().Unix())
	return cache
}

func (c *imageDiskCache) matches(dir string, ttl time.Duration, maxBytes int64) bool {
	if maxBytes < 0 {
		maxBytes = 0
	}
	return c != nil && c.dir == strings.TrimSpace(dir) && c.ttl == ttl && c.maxBytes == maxBytes
}

func imageCacheKey(nodeName string, target *url.URL) string {
	if target == nil {
		return ""
	}
	normalized := *target
	q := normalized.Query()
	for key := range q {
		if imageCacheIgnoredQueryParams[strings.ToLower(key)] {
			delete(q, key)
		}
	}
	normalized.RawQuery = q.Encode()
	return strings.ToLower(strings.TrimSpace(nodeName)) + "\n" + normalized.String()
}

func imageCacheKeyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", sum[:])
}

// get 查缓存，同时按「一个请求一次」记命中率：
//   - 缓存未启用（c==nil）、非 GET/HEAD、带 Range 的请求根本不进统计，
//     免得把分母稀释成没有意义的数字；
//   - 条目新鲜且能取到 body（含返回 304 的情况）算命中；
//   - 条目不存在、TTL 过期、只剩 meta 或只剩 body 的残缺条目算未命中。
func (c *imageDiskCache) get(r *http.Request, key string, reqOrigin string, env config.ProxyEnv) (res *http.Response, hit bool) {
	if c == nil || key == "" || r == nil || !imageCacheLookupMethod(r.Method) || r.Header.Get("Range") != "" {
		return nil, false
	}
	defer func() {
		if hit {
			c.hits.Add(1)
		} else {
			c.misses.Add(1)
		}
	}()
	paths := c.paths(key)
	meta, ok := c.readMeta(paths, key)
	if !ok {
		return nil, false
	}
	if c.expired(meta, c.now()) {
		c.remove(paths)
		return nil, false
	}
	// LRU：命中只更新内存里的访问时间，不落盘、不 Chtimes、不 fsync。
	c.indexTouch(paths.hash)

	headers := cloneHeader(meta.Header)
	addCORSHeaders(headers, reqOrigin, env)
	headers.Set("Access-Control-Expose-Headers", "Accept-Ranges, Content-Range, Content-Length, Content-Type")
	headers.Del("Vary")
	if imageClientCacheFresh(r, headers) {
		if !c.cachedBodyExists(paths) {
			c.remove(paths)
			return nil, false
		}
		headers.Del("Content-Length")
		return textResponse(http.StatusNotModified, "", headers), true
	}
	if strings.EqualFold(r.Method, http.MethodHead) {
		if !c.cachedBodyExists(paths) {
			c.remove(paths)
			return nil, false
		}
		return textResponse(meta.Status, "", headers), true
	}
	body, err := os.Open(paths.body)
	if err != nil {
		c.remove(paths)
		return nil, false
	}
	return &http.Response{
		StatusCode: meta.Status,
		Status:     fmt.Sprintf("%d %s", meta.Status, http.StatusText(meta.Status)),
		Header:     headers,
		Body:       body,
	}, true
}

func (c *imageDiskCache) wrapStore(r *http.Request, key string, res *http.Response, headers http.Header, onDone ...func()) bool {
	if c == nil || key == "" || r == nil || res == nil || res.Body == nil {
		return false
	}
	if !strings.EqualFold(r.Method, http.MethodGet) || r.Header.Get("Range") != "" || res.StatusCode != http.StatusOK {
		return false
	}
	if !imageCacheableContent(headers) {
		return false
	}
	paths := c.paths(key)
	if err := os.MkdirAll(paths.dir, 0o755); err != nil {
		return false
	}
	tmp, err := os.CreateTemp(paths.dir, filepath.Base(paths.body)+".*.tmp")
	if err != nil {
		return false
	}
	var done func()
	if len(onDone) > 0 {
		done = onDone[0]
	}
	now := c.now().Unix()
	meta := imageCacheMeta{
		KeyHash:   paths.hash,
		Status:    res.StatusCode,
		Header:    imageCacheStoredHeaders(headers),
		CreatedAt: now,
		ExpiresAt: now + int64(c.ttl.Seconds()),
	}
	res.Body = &imageCacheWriteCloser{
		cache:    c,
		src:      res.Body,
		file:     tmp,
		keyHash:  paths.hash,
		tmpBody:  tmp.Name(),
		bodyPath: paths.body,
		metaPath: paths.meta,
		meta:     meta,
		onDone:   done,
	}
	return true
}

func (c *imageDiskCache) CleanupExpired() {
	if c == nil {
		return
	}
	c.cleanupMu.Lock()
	defer c.cleanupMu.Unlock()
	now := c.now()
	if !c.lastCleanup.IsZero() && now.Sub(c.lastCleanup) < time.Hour {
		return
	}
	c.lastCleanup = now
	_ = filepath.WalkDir(c.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".tmp") {
			if info, statErr := d.Info(); statErr == nil && now.Sub(info.ModTime()) > time.Hour {
				_ = os.Remove(path)
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		var meta imageCacheMeta
		if json.Unmarshal(data, &meta) != nil || c.expired(meta, now) {
			body := strings.TrimSuffix(path, ".json") + ".body"
			_ = os.Remove(path)
			_ = os.Remove(body)
			hash := strings.TrimSuffix(d.Name(), ".json")
			c.deleteCachedMeta(hash)
			c.indexDelete(hash)
		}
		return nil
	})
}

func (h *Handler) ImageCacheStats(ctx context.Context) (ImageCacheStats, error) {
	cache, enabled, dir := h.imageCacheForOps(ctx)
	if cache == nil {
		return ImageCacheStats{Enabled: enabled, Dir: dir}, nil
	}
	stats, err := cache.Stats(enabled)
	if stats.Dir == "" {
		stats.Dir = dir
	}
	return stats, err
}

func (h *Handler) ClearImageCache(ctx context.Context) (ImageCacheStats, error) {
	cache, enabled, dir := h.imageCacheForOps(ctx)
	if cache == nil {
		return ImageCacheStats{Enabled: enabled, Dir: dir}, nil
	}
	clearErr := cache.Clear()
	stats, statErr := cache.Stats(enabled)
	if stats.Dir == "" {
		stats.Dir = dir
	}
	if clearErr != nil {
		return stats, clearErr
	}
	return stats, statErr
}

func (h *Handler) imageCacheForOps(ctx context.Context) (*imageDiskCache, bool, string) {
	if h == nil {
		return nil, false, ""
	}
	sys := h.systemConfig(ctx)
	dir := imageCacheDir(h.cfg)
	ttl := imageCacheTTL(sys)
	if sys.ImageCacheEnabled {
		return h.ensureImageCache(ctx), true, dir
	}
	h.imageCacheMu.Lock()
	cache := h.imageCache
	h.imageCache = nil
	h.imageCacheMu.Unlock()
	if cache != nil {
		cache.clearCachedMeta()
	}
	return newImageDiskCache(dir, ttl, sys.ImageCacheMaxBytes()), false, dir
}

func (c *imageDiskCache) Stats(enabled bool) (ImageCacheStats, error) {
	stats := ImageCacheStats{Enabled: enabled}
	if c == nil {
		return stats, nil
	}
	stats.Dir = c.dir
	stats.MaxBytes = c.maxBytes
	_, stats.IndexReady = c.indexUsage()
	c.cleanupMu.Lock()
	defer c.cleanupMu.Unlock()
	err := filepath.WalkDir(c.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d == nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		stats.Files++
		stats.Bytes += info.Size()
		if strings.HasSuffix(d.Name(), ".body") {
			stats.Entries++
		}
		return nil
	})
	if stats.MaxBytes > 0 {
		stats.Usage = float64(stats.Bytes) / float64(stats.MaxBytes)
	}
	// 缓存关闭时拿到的是临时实例，命中率没有统计口径可言，一律留空（lookups=0）。
	if enabled {
		c.fillHitStats(&stats)
	}
	if errors.Is(err, os.ErrNotExist) {
		return stats, nil
	}
	return stats, err
}

// fillHitStats 把命中率计数器填进统计结果。分母为 0 时命中率固定为 0，
// 不会产生 NaN（NaN 连 json.Marshal 都过不去），前端用 lookups==0 判断「暂无数据」。
func (c *imageDiskCache) fillHitStats(stats *ImageCacheStats) {
	if c == nil || stats == nil {
		return
	}
	stats.Hits = c.hits.Load()
	stats.Misses = c.misses.Load()
	stats.Lookups = stats.Hits + stats.Misses
	if stats.Lookups > 0 {
		stats.HitRate = float64(stats.Hits) / float64(stats.Lookups)
	}
	stats.StatsSince = c.statsSince.Load()
}

// resetStats 归零命中率计数器，并把统计起点推到当前时刻。
func (c *imageDiskCache) resetStats() {
	if c == nil {
		return
	}
	c.hits.Store(0)
	c.misses.Store(0)
	c.statsSince.Store(c.now().Unix())
}

// rollbackMiss 撤回一次未命中，且不会把计数器减成负数。
func (c *imageDiskCache) rollbackMiss() {
	if c == nil {
		return
	}
	for {
		cur := c.misses.Load()
		if cur <= 0 {
			return
		}
		if c.misses.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// Clear 清空缓存时一并重置命中率计数器：缓存已经空了，
// 清空前那段命中率描述的是一个不复存在的缓存状态，留着只会误导；
// 重置后 statsSince 指向清空时刻，前端能明确说出统计窗口。
func (c *imageDiskCache) Clear() error {
	if c == nil {
		return nil
	}
	c.cleanupMu.Lock()
	defer c.cleanupMu.Unlock()
	entries, err := os.ReadDir(c.dir)
	if errors.Is(err, os.ErrNotExist) {
		c.clearCachedMeta()
		c.indexReset()
		c.resetStats()
		c.lastCleanup = time.Time{}
		return nil
	}
	if err != nil {
		return err
	}
	var firstErr error
	for _, entry := range entries {
		if removeErr := os.RemoveAll(filepath.Join(c.dir, entry.Name())); removeErr != nil && firstErr == nil {
			firstErr = removeErr
		}
	}
	c.clearCachedMeta()
	c.indexReset()
	c.resetStats()
	c.lastCleanup = time.Time{}
	return firstErr
}

// ---------- 容量上限 + LRU 淘汰 ----------
//
// 索引全在内存里：hash -> {占用字节, 最近访问时间}。缓存命中时只写内存时间戳，
// 热路径不产生任何额外磁盘 IO；索引本身可丢，丢了重扫目录即可。

// startIndexScan 在后台重建索引，不阻塞启动。扫完之前不做任何淘汰，
// 避免索引不全时误删还热着的条目。
func (c *imageDiskCache) startIndexScan() {
	if c == nil || c.maxBytes <= 0 {
		return
	}
	c.scanOnce.Do(func() {
		go func() {
			c.scanIndex()
			close(c.scanDone)
		}()
	})
}

// scanIndex 扫描缓存目录重建索引。lastAccess 用文件 mtime 兜底
// （不依赖 atime：容器挂载普遍是 relatime/noatime）。
func (c *imageDiskCache) scanIndex() {
	if c == nil || c.maxBytes <= 0 {
		return
	}
	c.indexMu.Lock()
	gen := c.indexGen
	c.indexMu.Unlock()

	type scannedEntry struct {
		size    int64
		modTime int64
		hasBody bool
		hasMeta bool
	}
	found := map[string]*scannedEntry{}
	_ = filepath.WalkDir(c.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		isBody := strings.HasSuffix(name, ".body")
		if !isBody && !strings.HasSuffix(name, ".json") {
			return nil // .tmp 之类交给 CleanupExpired
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		hash := strings.TrimSuffix(strings.TrimSuffix(name, ".body"), ".json")
		entry := found[hash]
		if entry == nil {
			entry = &scannedEntry{}
			found[hash] = entry
		}
		entry.size += info.Size()
		if mod := info.ModTime().UnixNano(); mod > entry.modTime {
			entry.modTime = mod
		}
		if isBody {
			entry.hasBody = true
		} else {
			entry.hasMeta = true
		}
		return nil
	})

	now := c.now()
	for hash, entry := range found {
		if entry.hasBody && entry.hasMeta {
			continue
		}
		// 残缺条目（只剩 body 或只剩 meta）永远命不中：老于宽限期直接删；
		// 还很新的可能是正在写入的条目，先留着并计入占用，下次扫描再说。
		if now.Sub(time.Unix(0, entry.modTime)) > imageCacheOrphanGrace {
			c.remove(c.pathsForHash(hash))
			delete(found, hash)
		}
	}

	c.indexMu.Lock()
	if c.indexGen != gen {
		c.indexMu.Unlock() // 扫描期间缓存被清空，本次结果作废
		return
	}
	for hash, entry := range found {
		existing := c.index[hash]
		if existing == nil {
			c.index[hash] = &imageCacheIndexEntry{size: entry.size, lastAccess: entry.modTime}
			c.indexBytes += entry.size
			continue
		}
		// 扫描期间被写入或命中过的条目：访问时间以内存里的为准，只补体积。
		if existing.size == 0 {
			existing.size = entry.size
			c.indexBytes += entry.size
		}
		if entry.modTime > existing.lastAccess {
			existing.lastAccess = entry.modTime
		}
	}
	c.indexReady = true
	c.indexMu.Unlock()
	c.evictOverflow()
}

func (c *imageDiskCache) indexTouch(hash string) {
	if c == nil || c.maxBytes <= 0 || hash == "" {
		return
	}
	now := c.now().UnixNano()
	c.indexMu.Lock()
	if entry := c.index[hash]; entry != nil {
		entry.lastAccess = now
	} else if !c.indexReady {
		// 索引还没建好就命中：先把访问时间记下来，体积等扫描补齐，
		// 免得刚被访问过的条目扫描完就因为 mtime 很旧而被淘汰。
		c.index[hash] = &imageCacheIndexEntry{lastAccess: now}
	}
	c.indexMu.Unlock()
}

func (c *imageDiskCache) indexPut(hash string, size int64) {
	if c == nil || c.maxBytes <= 0 || hash == "" || size <= 0 {
		return
	}
	now := c.now().UnixNano()
	c.indexMu.Lock()
	if entry := c.index[hash]; entry != nil {
		c.indexBytes += size - entry.size
		entry.size = size
		entry.lastAccess = now
	} else {
		c.index[hash] = &imageCacheIndexEntry{size: size, lastAccess: now}
		c.indexBytes += size
	}
	c.indexMu.Unlock()
}

func (c *imageDiskCache) indexDelete(hash string) {
	if c == nil || c.maxBytes <= 0 || hash == "" {
		return
	}
	c.indexMu.Lock()
	if entry := c.index[hash]; entry != nil {
		c.indexBytes -= entry.size
		if c.indexBytes < 0 {
			c.indexBytes = 0
		}
		delete(c.index, hash)
	}
	c.indexMu.Unlock()
}

func (c *imageDiskCache) indexReset() {
	if c == nil {
		return
	}
	c.indexMu.Lock()
	c.indexGen++
	c.index = map[string]*imageCacheIndexEntry{}
	c.indexBytes = 0
	c.indexMu.Unlock()
}

// maybeEvict 只在确实超上限时才启动一次后台淘汰，写入路径上不等待。
func (c *imageDiskCache) maybeEvict() {
	if c == nil || c.maxBytes <= 0 {
		return
	}
	c.indexMu.Lock()
	over := c.indexReady && c.indexBytes > c.maxBytes
	c.indexMu.Unlock()
	if !over || !c.evicting.CompareAndSwap(false, true) {
		return
	}
	go func() {
		for {
			c.evictOverflow()
			c.evicting.Store(false)
			// 让出标志位后再确认一次：淘汰期间到达的写入会被 CAS 挡掉，
			// 不复查的话就会一直停在上限之上。
			c.indexMu.Lock()
			again := c.indexReady && c.indexBytes > c.maxBytes
			c.indexMu.Unlock()
			if !again || !c.evicting.CompareAndSwap(false, true) {
				return
			}
		}
	}()
}

// evictOverflow 按 lastAccess 从旧到新淘汰，一直淘汰到低水位（上限的 90%）。
// 选victim 在锁内完成，实际删文件放到锁外，不让淘汰阻塞读写。
func (c *imageDiskCache) evictOverflow() {
	if c == nil || c.maxBytes <= 0 {
		return
	}
	target := c.maxBytes * imageCacheEvictLowWaterPct / 100
	c.indexMu.Lock()
	if !c.indexReady || c.indexBytes <= c.maxBytes {
		c.indexMu.Unlock()
		return
	}
	type candidate struct {
		hash       string
		lastAccess int64
	}
	candidates := make([]candidate, 0, len(c.index))
	for hash, entry := range c.index {
		candidates = append(candidates, candidate{hash: hash, lastAccess: entry.lastAccess})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].lastAccess != candidates[j].lastAccess {
			return candidates[i].lastAccess < candidates[j].lastAccess
		}
		return candidates[i].hash < candidates[j].hash
	})
	victims := make([]string, 0, 16)
	for _, cand := range candidates {
		if c.indexBytes <= target {
			break
		}
		entry := c.index[cand.hash]
		if entry == nil {
			continue
		}
		c.indexBytes -= entry.size
		if c.indexBytes < 0 {
			c.indexBytes = 0
		}
		delete(c.index, cand.hash)
		victims = append(victims, cand.hash)
	}
	c.indexMu.Unlock()

	for _, hash := range victims {
		c.remove(c.pathsForHash(hash))
	}
}

func (c *imageDiskCache) indexUsage() (bytes int64, ready bool) {
	if c == nil {
		return 0, false
	}
	c.indexMu.Lock()
	defer c.indexMu.Unlock()
	return c.indexBytes, c.indexReady
}

func (c *imageDiskCache) readMeta(paths imageCachePaths, key string) (imageCacheMeta, bool) {
	now := c.now()
	if meta, ok := c.cachedMeta(paths.hash, now); ok {
		return meta, true
	}
	data, err := os.ReadFile(paths.meta)
	if err != nil {
		return imageCacheMeta{}, false
	}
	var meta imageCacheMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		c.remove(paths)
		return imageCacheMeta{}, false
	}
	if meta.KeyHash != imageCacheKeyHash(key) || meta.Status == 0 {
		c.remove(paths)
		return imageCacheMeta{}, false
	}
	if !c.cachedBodyExists(paths) {
		c.remove(paths)
		return imageCacheMeta{}, false
	}
	c.setCachedMeta(paths.hash, meta, now)
	return meta, true
}

func (c *imageDiskCache) expired(meta imageCacheMeta, now time.Time) bool {
	nowUnix := now.Unix()
	ttlSeconds := int64(c.ttl.Seconds())
	if meta.CreatedAt > 0 && ttlSeconds > 0 {
		return meta.CreatedAt+ttlSeconds <= nowUnix
	}
	if meta.ExpiresAt > 0 {
		return meta.ExpiresAt <= nowUnix
	}
	return true
}

func (c *imageDiskCache) paths(key string) imageCachePaths {
	hex := imageCacheKeyHash(key)
	return c.pathsForHash(hex)
}

func (c *imageDiskCache) pathsForHash(hex string) imageCachePaths {
	dir := filepath.Join(c.dir, hex[:2])
	base := filepath.Join(dir, hex)
	return imageCachePaths{hash: hex, dir: dir, body: base + ".body", meta: base + ".json"}
}

func (c *imageDiskCache) remove(paths imageCachePaths) {
	_ = os.Remove(paths.meta)
	_ = os.Remove(paths.body)
	c.deleteCachedMeta(paths.hash)
	c.indexDelete(paths.hash)
}

func (c *imageDiskCache) cachedBodyExists(paths imageCachePaths) bool {
	_, err := os.Stat(paths.body)
	return err == nil
}

func (c *imageDiskCache) beginFill(key string) (*imageCacheFill, bool) {
	if c == nil || key == "" {
		return nil, true
	}
	hash := imageCacheKeyHash(key)
	c.fillMu.Lock()
	defer c.fillMu.Unlock()
	if c.fills == nil {
		c.fills = map[string]*imageCacheFill{}
	}
	if fill := c.fills[hash]; fill != nil {
		return fill, false
	}
	fill := &imageCacheFill{hash: hash, done: make(chan struct{})}
	c.fills[hash] = fill
	return fill, true
}

func (c *imageDiskCache) waitFill(ctx context.Context, fill *imageCacheFill) error {
	if fill == nil {
		return nil
	}
	// 合并回源：本请求刚才那次查询已经记了一次未命中，但它等的是别人正在回源的同一张图，
	// 等完还会再查一次缓存。这里把那次未命中撤回，让一个请求只落一次命中/未命中，
	// 否则被合并的请求会同时贡献一次未命中和一次命中，把命中率算歪。
	c.rollbackMiss()
	select {
	case <-fill.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *imageDiskCache) finishFill(fill *imageCacheFill) {
	if c == nil || fill == nil {
		return
	}
	fill.once.Do(func() {
		c.fillMu.Lock()
		if c.fills[fill.hash] == fill {
			delete(c.fills, fill.hash)
		}
		c.fillMu.Unlock()
		close(fill.done)
	})
}

func (c *imageDiskCache) cachedMeta(hash string, now time.Time) (imageCacheMeta, bool) {
	if c == nil || hash == "" {
		return imageCacheMeta{}, false
	}
	c.metaMu.RLock()
	entry, ok := c.metaCache[hash]
	c.metaMu.RUnlock()
	if !ok {
		return imageCacheMeta{}, false
	}
	if now.After(entry.exp) {
		c.deleteCachedMeta(hash)
		return imageCacheMeta{}, false
	}
	return cloneImageCacheMeta(entry.meta), true
}

func (c *imageDiskCache) setCachedMeta(hash string, meta imageCacheMeta, now time.Time) {
	if c == nil || hash == "" {
		return
	}
	c.metaMu.Lock()
	defer c.metaMu.Unlock()
	if c.metaCache == nil {
		c.metaCache = map[string]imageMetaCacheEntry{}
	}
	c.metaCache[hash] = imageMetaCacheEntry{meta: cloneImageCacheMeta(meta), exp: now.Add(imageCacheMetaCacheTTL)}
	if len(c.metaCache) <= imageCacheMetaCacheMaxItems {
		return
	}
	for key, entry := range c.metaCache {
		if now.After(entry.exp) {
			delete(c.metaCache, key)
		}
	}
	for len(c.metaCache) > imageCacheMetaCacheMaxItems {
		for key := range c.metaCache {
			delete(c.metaCache, key)
			break
		}
	}
}

func (c *imageDiskCache) deleteCachedMeta(hash string) {
	if c == nil || hash == "" {
		return
	}
	c.metaMu.Lock()
	delete(c.metaCache, hash)
	c.metaMu.Unlock()
}

func (c *imageDiskCache) clearCachedMeta() {
	if c == nil {
		return
	}
	c.metaMu.Lock()
	c.metaCache = nil
	c.metaMu.Unlock()
}

func cloneImageCacheMeta(meta imageCacheMeta) imageCacheMeta {
	meta.Header = cloneHeader(meta.Header)
	return meta
}

type imageCacheWriteCloser struct {
	cache    *imageDiskCache
	src      io.ReadCloser
	file     *os.File
	keyHash  string
	tmpBody  string
	bodyPath string
	metaPath string
	meta     imageCacheMeta
	onDone   func()
	done     bool
	failed   bool
	written  int64
}

func (w *imageCacheWriteCloser) Read(p []byte) (int, error) {
	n, err := w.src.Read(p)
	if n > 0 && !w.failed {
		if _, writeErr := w.file.Write(p[:n]); writeErr != nil {
			w.failed = true
		} else {
			w.written += int64(n)
		}
	}
	if err == io.EOF {
		w.commit()
	}
	return n, err
}

func (w *imageCacheWriteCloser) Close() error {
	if !w.done {
		w.abort()
	}
	return w.src.Close()
}

func (w *imageCacheWriteCloser) commit() {
	if w.done {
		return
	}
	w.done = true
	defer w.finish()
	if w.failed {
		_ = w.file.Close()
		_ = os.Remove(w.tmpBody)
		return
	}
	if err := w.file.Close(); err != nil {
		_ = os.Remove(w.tmpBody)
		return
	}
	if err := os.Rename(w.tmpBody, w.bodyPath); err != nil {
		_ = os.Remove(w.tmpBody)
		return
	}
	data, err := json.Marshal(w.meta)
	if err != nil {
		_ = os.Remove(w.bodyPath)
		return
	}
	tmpMeta := w.metaPath + "." + strconv.FormatInt(time.Now().UnixNano(), 10) + ".tmp"
	if err := os.WriteFile(tmpMeta, data, 0o644); err != nil {
		_ = os.Remove(w.bodyPath)
		return
	}
	if err := os.Rename(tmpMeta, w.metaPath); err != nil {
		_ = os.Remove(tmpMeta)
		_ = os.Remove(w.bodyPath)
		return
	}
	if w.cache != nil {
		w.cache.setCachedMeta(w.keyHash, w.meta, w.cache.now())
		w.cache.indexPut(w.keyHash, w.written+int64(len(data)))
		w.cache.maybeEvict()
	}
}

func (w *imageCacheWriteCloser) abort() {
	w.done = true
	_ = w.file.Close()
	_ = os.Remove(w.tmpBody)
	w.finish()
}

func (w *imageCacheWriteCloser) finish() {
	if w.onDone == nil {
		return
	}
	done := w.onDone
	w.onDone = nil
	done()
}

func imageCacheLookupMethod(method string) bool {
	return strings.EqualFold(method, http.MethodGet) || strings.EqualFold(method, http.MethodHead)
}

func imageCacheableContent(headers http.Header) bool {
	contentType := strings.ToLower(strings.TrimSpace(headers.Get("Content-Type")))
	if contentType == "" {
		return true
	}
	return strings.HasPrefix(contentType, "image/")
}

func imageCacheStoredHeaders(headers http.Header) http.Header {
	out := cloneHeader(headers)
	deleteHeaders(out,
		"Access-Control-Allow-Credentials",
		"Access-Control-Allow-Origin",
		"Access-Control-Expose-Headers",
		"Content-Length",
		"Set-Cookie",
		"Transfer-Encoding",
		"Vary",
	)
	return out
}

func imageClientCacheFresh(r *http.Request, headers http.Header) bool {
	if r == nil {
		return false
	}
	etag := strings.TrimSpace(headers.Get("ETag"))
	if etag != "" && imageETagMatches(r.Header.Get("If-None-Match"), etag) {
		return true
	}
	ifModifiedSince := strings.TrimSpace(r.Header.Get("If-Modified-Since"))
	lastModified := strings.TrimSpace(headers.Get("Last-Modified"))
	if ifModifiedSince == "" || lastModified == "" {
		return false
	}
	clientTime, err := http.ParseTime(ifModifiedSince)
	if err != nil {
		return false
	}
	cacheTime, err := http.ParseTime(lastModified)
	if err != nil {
		return false
	}
	return !cacheTime.After(clientTime)
}

func imageETagMatches(ifNoneMatch string, etag string) bool {
	ifNoneMatch = strings.TrimSpace(ifNoneMatch)
	if ifNoneMatch == "" {
		return false
	}
	if ifNoneMatch == "*" {
		return true
	}
	for _, value := range strings.Split(ifNoneMatch, ",") {
		if strings.TrimSpace(value) == etag {
			return true
		}
	}
	return false
}
