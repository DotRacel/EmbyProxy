package proxy

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"embyproxy/internal/config"
	"embyproxy/internal/logging"
)

// storeImageCacheEntryWithHeaders 写入一条带自定义响应头（例如 ETag）的缓存条目。
func storeImageCacheEntryWithHeaders(t *testing.T, cache *imageDiskCache, name string, body []byte, headers http.Header) {
	t.Helper()
	key := imageCacheTestKey(name)
	res := bytesResponse(http.StatusOK, body, headers)
	if !cache.wrapStore(httptestRequest(http.MethodGet), key, res, headers) {
		t.Fatalf("wrapStore(%s) 没有接管响应体", name)
	}
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
}

func assertHitStats(t *testing.T, cache *imageDiskCache, wantHits int64, wantMisses int64) {
	t.Helper()
	stats, err := cache.Stats(true)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Hits != wantHits || stats.Misses != wantMisses {
		t.Fatalf("命中统计 = %d 命中 / %d 未命中，期望 %d / %d", stats.Hits, stats.Misses, wantHits, wantMisses)
	}
	if stats.Lookups != wantHits+wantMisses {
		t.Fatalf("stats.Lookups = %d，期望 %d", stats.Lookups, wantHits+wantMisses)
	}
	if math.IsNaN(stats.HitRate) || math.IsInf(stats.HitRate, 0) {
		t.Fatalf("stats.HitRate = %v，不能是 NaN/Inf", stats.HitRate)
	}
	want := 0.0
	if stats.Lookups > 0 {
		want = float64(wantHits) / float64(stats.Lookups)
	}
	if stats.HitRate != want {
		t.Fatalf("stats.HitRate = %v，期望 %v", stats.HitRate, want)
	}
}

// 冷查未命中、写入本身不计数、再查命中。
func TestImageCacheHitStatsCountsHitsAndMisses(t *testing.T) {
	cache := newImageDiskCache(t.TempDir(), time.Hour, 0)
	body := make([]byte, 1024)

	if readImageCacheEntry(t, cache, "a") {
		t.Fatal("空缓存竟然命中了")
	}
	assertHitStats(t, cache, 0, 1)

	storeImageCacheEntry(t, cache, "a", body)
	assertHitStats(t, cache, 0, 1) // 写入不进命中率统计

	if !readImageCacheEntry(t, cache, "a") {
		t.Fatal("写入后仍未命中")
	}
	assertHitStats(t, cache, 1, 1)

	stats, err := cache.Stats(true)
	if err != nil {
		t.Fatal(err)
	}
	if stats.HitRate != 0.5 {
		t.Fatalf("stats.HitRate = %v，期望 0.5", stats.HitRate)
	}
	if stats.StatsSince <= 0 {
		t.Fatalf("stats.StatsSince = %d，期望统计起点时间戳", stats.StatsSince)
	}
}

// TTL 过期（读时惰性失效）算未命中。
func TestImageCacheExpiredEntryCountsAsMiss(t *testing.T) {
	clock := newStepClock()
	cache := newImageDiskCache(t.TempDir(), time.Hour, 0)
	cache.now = clock.Now
	storeImageCacheEntry(t, cache, "a", make([]byte, 512))

	clock.advance(2 * time.Hour)
	// meta 内存缓存的 TTL 是 1 分钟，推进 2 小时后必然从磁盘重读。
	if readImageCacheEntry(t, cache, "a") {
		t.Fatal("过期条目仍然命中")
	}
	assertHitStats(t, cache, 0, 1)
}

// 条目新鲜但客户端带了 If-None-Match，返回 304，算命中。
func TestImageCacheNotModifiedCountsAsHit(t *testing.T) {
	cache := newImageDiskCache(t.TempDir(), time.Hour, 0)
	headers := http.Header{
		"Content-Type": []string{"image/jpeg"},
		"Etag":         []string{`"abc123"`},
	}
	storeImageCacheEntryWithHeaders(t, cache, "a", make([]byte, 256), headers)

	req := httptestRequest(http.MethodGet)
	req.Header.Set("If-None-Match", `"abc123"`)
	res, ok := cache.get(req, imageCacheTestKey("a"), "", config.ProxyEnv{})
	if !ok {
		t.Fatal("带 If-None-Match 的请求未命中缓存")
	}
	if res.Body != nil {
		_ = res.Body.Close()
	}
	if res.StatusCode != http.StatusNotModified {
		t.Fatalf("状态码 = %d，期望 304", res.StatusCode)
	}
	assertHitStats(t, cache, 1, 0)
}

// 只剩 meta（body 被删）的残缺条目算未命中。
func TestImageCacheOrphanEntryCountsAsMiss(t *testing.T) {
	cache := newImageDiskCache(t.TempDir(), time.Hour, 0)
	storeImageCacheEntry(t, cache, "a", make([]byte, 256))
	paths := cache.paths(imageCacheTestKey("a"))
	if err := os.Remove(paths.body); err != nil {
		t.Fatal(err)
	}
	cache.clearCachedMeta() // 绕开 meta 内存缓存，模拟进程重启后读到残缺条目

	if readImageCacheEntry(t, cache, "a") {
		t.Fatal("残缺条目竟然命中")
	}
	assertHitStats(t, cache, 0, 1)
}

// 不查缓存的请求形态（非 GET/HEAD、带 Range、空 key）不进入命中率统计。
func TestImageCacheHitStatsSkipsNonLookupRequests(t *testing.T) {
	cache := newImageDiskCache(t.TempDir(), time.Hour, 0)
	storeImageCacheEntry(t, cache, "a", make([]byte, 256))
	key := imageCacheTestKey("a")

	rangeReq := httptestRequest(http.MethodGet)
	rangeReq.Header.Set("Range", "bytes=0-99")
	if _, ok := cache.get(rangeReq, key, "", config.ProxyEnv{}); ok {
		t.Fatal("Range 请求不应该命中缓存")
	}
	if _, ok := cache.get(httptestRequest(http.MethodPost), key, "", config.ProxyEnv{}); ok {
		t.Fatal("POST 请求不应该命中缓存")
	}
	if _, ok := cache.get(httptestRequest(http.MethodGet), "", "", config.ProxyEnv{}); ok {
		t.Fatal("空 key 不应该命中缓存")
	}
	assertHitStats(t, cache, 0, 0)

	stats, err := cache.Stats(true)
	if err != nil {
		t.Fatal(err)
	}
	if stats.HitRate != 0 || stats.Lookups != 0 {
		t.Fatalf("无样本时 stats = %+v，期望 lookups=0 hitRate=0", stats)
	}
	data, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("统计结果无法序列化（NaN？）: %v", err)
	}
	if !strings.Contains(string(data), `"hitRate":0`) {
		t.Fatalf("序列化结果缺少 hitRate=0: %s", data)
	}
}

// 缓存关闭时的请求根本不计数：接口给出的样本数保持 0，命中率不被稀释。
func TestImageCacheDisabledDoesNotCount(t *testing.T) {
	ctx := context.Background()
	h := New(config.Config{CWD: t.TempDir()}, newProxyTestStore(t), nil, logging.New("silent", false))

	cache := h.ensureImageCache(ctx)
	if cache != nil {
		t.Fatal("默认配置下图片缓存应当是关闭的")
	}
	for i := 0; i < 5; i++ {
		if _, ok := cache.get(httptestRequest(http.MethodGet), imageCacheTestKey("a"), "", config.ProxyEnv{}); ok {
			t.Fatal("关闭的缓存不应该命中")
		}
	}

	stats, err := h.ImageCacheStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Enabled {
		t.Fatalf("stats.Enabled = true，期望 false: %+v", stats)
	}
	if stats.Hits != 0 || stats.Misses != 0 || stats.Lookups != 0 || stats.HitRate != 0 {
		t.Fatalf("缓存关闭时 stats = %+v，期望命中率相关字段全为 0", stats)
	}
	if math.IsNaN(stats.HitRate) {
		t.Fatal("stats.HitRate 是 NaN")
	}
}

// 合并回源（同一张图并发回源，后来者等 leader）的请求只能计一次。
func TestImageCacheCoalescedWaitCountsOnce(t *testing.T) {
	cache := newImageDiskCache(t.TempDir(), time.Hour, 0)
	key := imageCacheTestKey("a")

	// 请求 1：未命中，成为回源 leader。
	if readImageCacheEntry(t, cache, "a") {
		t.Fatal("空缓存竟然命中")
	}
	fill, leader := cache.beginFill(key)
	if !leader {
		t.Fatal("第一个请求应当是 leader")
	}

	// 请求 2：未命中，发现已有人在回源，转为等待。
	if readImageCacheEntry(t, cache, "a") {
		t.Fatal("空缓存竟然命中")
	}
	waitFill, waitLeader := cache.beginFill(key)
	if waitLeader {
		t.Fatal("第二个请求不应当是 leader")
	}

	// leader 回源完成并落盘。
	storeImageCacheEntry(t, cache, "a", make([]byte, 256))
	cache.finishFill(fill)

	if err := cache.waitFill(context.Background(), waitFill); err != nil {
		t.Fatal(err)
	}
	if !readImageCacheEntry(t, cache, "a") {
		t.Fatal("等待回源结束后仍未命中")
	}

	// 两个请求：leader 一次未命中，等待者一次命中。
	assertHitStats(t, cache, 1, 1)
}

// Clear 清空缓存时命中率计数器一并归零，统计起点推到清空时刻。
func TestImageCacheClearResetsHitStats(t *testing.T) {
	clock := newStepClock()
	cache := newImageDiskCache(t.TempDir(), time.Hour, 0)
	cache.now = clock.Now
	cache.statsSince.Store(clock.Now().Unix())
	storeImageCacheEntry(t, cache, "a", make([]byte, 256))
	if !readImageCacheEntry(t, cache, "a") {
		t.Fatal("写入后未命中")
	}
	if readImageCacheEntry(t, cache, "b") {
		t.Fatal("未写入的条目竟然命中")
	}
	assertHitStats(t, cache, 1, 1)

	before := cache.statsSince.Load()
	clock.advance(time.Minute)
	if err := cache.Clear(); err != nil {
		t.Fatal(err)
	}
	assertHitStats(t, cache, 0, 0)
	stats, err := cache.Stats(true)
	if err != nil {
		t.Fatal(err)
	}
	if stats.StatsSince <= before {
		t.Fatalf("Clear 后 statsSince = %d，期望晚于 %d", stats.StatsSince, before)
	}
}

// 并发下计数不能丢，也不能有数据竞争（go test -race）。
func TestImageCacheHitStatsConcurrentCountsAreExact(t *testing.T) {
	cache := newImageDiskCache(t.TempDir(), time.Hour, 1<<30)
	cache.scanIndex()
	storeImageCacheEntry(t, cache, "hit", make([]byte, 512))

	const workers = 16
	const rounds = 40
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				if i%2 == 0 {
					res, ok := cache.get(httptestRequest(http.MethodGet), imageCacheTestKey("hit"), "", config.ProxyEnv{})
					if !ok {
						t.Errorf("并发读取未命中")
						return
					}
					_, _ = io.Copy(io.Discard, res.Body)
					_ = res.Body.Close()
					continue
				}
				if _, ok := cache.get(httptestRequest(http.MethodGet), imageCacheTestKey("miss"), "", config.ProxyEnv{}); ok {
					t.Errorf("不存在的条目竟然命中")
					return
				}
			}
		}(i)
	}
	wg.Wait()

	want := int64(workers / 2 * rounds)
	assertHitStats(t, cache, want, want)
}
