package proxy

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"embyproxy/internal/config"
)

// stepClock 是可手动推进的时钟，多 goroutine 读安全（淘汰在后台跑）。
type stepClock struct {
	mu  sync.Mutex
	now time.Time
}

func newStepClock() *stepClock {
	return &stepClock{now: time.Unix(1_800_000_000, 0)}
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stepClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func imageCacheTestKey(name string) string {
	return "node\nhttps://upstream.example/emby/Items/" + name + "/Images/Primary?tag=v1"
}

func storeImageCacheEntry(t *testing.T, cache *imageDiskCache, name string, body []byte) {
	t.Helper()
	key := imageCacheTestKey(name)
	res := bytesResponse(http.StatusOK, body, http.Header{"Content-Type": []string{"image/jpeg"}})
	if !cache.wrapStore(httptestRequest(http.MethodGet), key, res, res.Header) {
		t.Fatalf("wrapStore(%s) 没有接管响应体", name)
	}
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if !cache.cachedBodyExists(cache.paths(key)) {
		t.Fatalf("写入 %s 后 body 文件不存在", name)
	}
}

func readImageCacheEntry(t *testing.T, cache *imageDiskCache, name string) bool {
	t.Helper()
	res, ok := cache.get(httptestRequest(http.MethodGet), imageCacheTestKey(name), "", config.ProxyEnv{})
	if !ok {
		return false
	}
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	return true
}

func imageCacheEntryOnDisk(cache *imageDiskCache, name string) bool {
	paths := cache.paths(imageCacheTestKey(name))
	if _, err := os.Stat(paths.body); err != nil {
		return false
	}
	_, err := os.Stat(paths.meta)
	return err == nil
}

// measureImageCacheEntrySize 量出一条缓存（body+meta）在索引里占多少字节，
// 让容量相关的断言不用写死魔法数字。
func measureImageCacheEntrySize(t *testing.T, body []byte) int64 {
	t.Helper()
	cache := newImageDiskCache(t.TempDir(), time.Hour, 1<<40)
	storeImageCacheEntry(t, cache, "probe", body)
	size, _ := cache.indexUsage()
	if size <= 0 {
		t.Fatalf("测量缓存条目大小失败: %d", size)
	}
	return size
}

func waitIndexBytes(t *testing.T, cache *imageDiskCache, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var got int64
	for time.Now().Before(deadline) {
		got, _ = cache.indexUsage()
		if got <= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待淘汰超时：索引占用 %d，期望 <= %d", got, want)
}

// 超过容量上限时按 LRU 淘汰：最近访问过的活下来，最久没访问的被删。
func TestImageCacheEvictsLeastRecentlyUsedOnOverflow(t *testing.T) {
	body := make([]byte, 4096)
	entrySize := measureImageCacheEntrySize(t, body)

	clock := newStepClock()
	cache := newImageDiskCache(t.TempDir(), time.Hour, 6*entrySize)
	cache.now = clock.Now
	cache.scanIndex() // 空目录，直接把索引置为就绪

	names := []string{"a", "b", "c", "d", "e", "f"}
	for _, name := range names {
		clock.advance(time.Second)
		storeImageCacheEntry(t, cache, name, body)
	}
	if bytes, _ := cache.indexUsage(); bytes != 6*entrySize {
		t.Fatalf("写满前索引占用 = %d，期望 %d", bytes, 6*entrySize)
	}
	for _, name := range names {
		if !imageCacheEntryOnDisk(cache, name) {
			t.Fatalf("没超上限就淘汰了 %s", name)
		}
	}

	// a 是最早写入的，但读一次让它变成最近访问；b 就成了最久未访问的那个。
	clock.advance(time.Second)
	if !readImageCacheEntry(t, cache, "a") {
		t.Fatal("读取 a 未命中")
	}

	clock.advance(time.Second)
	storeImageCacheEntry(t, cache, "g", body)

	target := (6 * entrySize) * imageCacheEvictLowWaterPct / 100
	waitIndexBytes(t, cache, target)

	if !imageCacheEntryOnDisk(cache, "a") {
		t.Fatal("最近访问过的 a 被淘汰了")
	}
	if imageCacheEntryOnDisk(cache, "b") {
		t.Fatal("最久未访问的 b 没有被淘汰")
	}
	if !imageCacheEntryOnDisk(cache, "g") {
		t.Fatal("刚写入的 g 被淘汰了")
	}
	if !readImageCacheEntry(t, cache, "a") {
		t.Fatal("淘汰后 a 读不到了")
	}
	if readImageCacheEntry(t, cache, "b") {
		t.Fatal("被淘汰的 b 仍然命中")
	}
}

// 淘汰要一路清到低水位（上限的 90%），而不是刚好压到上限就停。
func TestImageCacheEvictsDownToLowWatermark(t *testing.T) {
	body := make([]byte, 2048)
	entrySize := measureImageCacheEntrySize(t, body)

	clock := newStepClock()
	maxBytes := 20 * entrySize
	cache := newImageDiskCache(t.TempDir(), time.Hour, maxBytes)
	cache.now = clock.Now

	// 索引还没就绪时不淘汰，先把 25 条全部写进去，再一次性触发淘汰，
	// 这样断言的是「一轮淘汰到哪」而不是中间态。
	for i := 0; i < 25; i++ {
		clock.advance(time.Second)
		storeImageCacheEntry(t, cache, fmt.Sprintf("k%02d", i), body)
	}
	if bytes, ready := cache.indexUsage(); bytes != 25*entrySize || ready {
		t.Fatalf("扫描前索引 = (%d, ready=%v)，期望 (%d, ready=false)", bytes, ready, 25*entrySize)
	}

	cache.scanIndex() // 置为就绪并同步跑一轮淘汰

	target := maxBytes * imageCacheEvictLowWaterPct / 100
	bytes, ready := cache.indexUsage()
	if !ready {
		t.Fatal("扫描结束后索引仍未就绪")
	}
	if bytes > target {
		t.Fatalf("淘汰后占用 %d，没到低水位 %d", bytes, target)
	}
	if bytes <= target-entrySize {
		t.Fatalf("淘汰过头：占用 %d，低水位 %d，多删了至少一条", bytes, target)
	}
	if want := 18 * entrySize; bytes != want {
		t.Fatalf("淘汰后占用 = %d，期望 %d（上限 20 条 → 低水位 18 条）", bytes, want)
	}
	for i := 0; i < 25; i++ {
		name := fmt.Sprintf("k%02d", i)
		onDisk := imageCacheEntryOnDisk(cache, name)
		if i < 7 && onDisk {
			t.Fatalf("%s 属于最旧的 7 条，应该被淘汰", name)
		}
		if i >= 7 && !onDisk {
			t.Fatalf("%s 不该被淘汰", name)
		}
	}
}

// TTL 与容量是两个并存的维度：没到容量上限，过期的照样要清掉。
func TestImageCacheTTLStillAppliesUnderCapacity(t *testing.T) {
	body := make([]byte, 1024)
	entrySize := measureImageCacheEntrySize(t, body)

	clock := newStepClock()
	cache := newImageDiskCache(t.TempDir(), time.Hour, 100*entrySize)
	cache.now = clock.Now
	cache.scanIndex()

	for _, name := range []string{"a", "b", "c"} {
		storeImageCacheEntry(t, cache, name, body)
	}
	if bytes, _ := cache.indexUsage(); bytes != 3*entrySize {
		t.Fatalf("索引占用 = %d，期望 %d", bytes, 3*entrySize)
	}

	clock.advance(2 * time.Hour) // 超过 TTL，但离容量上限还远得很
	cache.CleanupExpired()

	for _, name := range []string{"a", "b", "c"} {
		if imageCacheEntryOnDisk(cache, name) {
			t.Fatalf("TTL 过期的 %s 没有被清理", name)
		}
		if readImageCacheEntry(t, cache, name) {
			t.Fatalf("TTL 过期的 %s 仍然命中", name)
		}
	}
	if bytes, _ := cache.indexUsage(); bytes != 0 {
		t.Fatalf("TTL 清理后索引占用 = %d，期望 0", bytes)
	}
}

// TTL 读时惰性失效也要把索引里的占用扣掉。
func TestImageCacheLazyExpiryUpdatesIndex(t *testing.T) {
	body := make([]byte, 1024)
	clock := newStepClock()
	cache := newImageDiskCache(t.TempDir(), time.Hour, 1<<20)
	cache.now = clock.Now
	cache.scanIndex()
	storeImageCacheEntry(t, cache, "a", body)

	clock.advance(2 * time.Hour)
	if readImageCacheEntry(t, cache, "a") {
		t.Fatal("过期条目仍然命中")
	}
	if bytes, _ := cache.indexUsage(); bytes != 0 {
		t.Fatalf("惰性失效后索引占用 = %d，期望 0", bytes)
	}
}

// 启动扫描重建索引：完整条目照单全收，残缺条目按新旧区别对待。
func TestImageCacheStartupScanRebuildsIndex(t *testing.T) {
	dir := t.TempDir()
	body := make([]byte, 3072)
	writer := newImageDiskCache(dir, time.Hour, 1<<40)
	storeImageCacheEntry(t, writer, "a", body)
	storeImageCacheEntry(t, writer, "b", body)
	completeBytes, _ := writer.indexUsage()

	// 只剩 body 的老残件：永远命不中，扫描时直接删。
	stalePaths := writer.pathsForHash(strings.Repeat("1", 64))
	if err := os.MkdirAll(stalePaths.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stalePaths.body, make([]byte, 512), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * imageCacheOrphanGrace)
	if err := os.Chtimes(stalePaths.body, old, old); err != nil {
		t.Fatal(err)
	}

	// 只剩 body 的新残件：可能是正在写入的条目，留着并计入占用。
	freshPaths := writer.pathsForHash(strings.Repeat("2", 64))
	if err := os.MkdirAll(freshPaths.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(freshPaths.body, make([]byte, 256), 0o644); err != nil {
		t.Fatal(err)
	}

	// 只剩 meta 的老残件：同样直接删。
	metaOnly := writer.pathsForHash(strings.Repeat("3", 64))
	if err := os.MkdirAll(metaOnly.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaOnly.meta, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(metaOnly.meta, old, old); err != nil {
		t.Fatal(err)
	}

	// 临时文件不该被算进索引。
	if err := os.WriteFile(filepath.Join(stalePaths.dir, "abcd.1234.tmp"), []byte("tmp"), 0o644); err != nil {
		t.Fatal(err)
	}

	cache := newImageDiskCache(dir, time.Hour, 1<<40)
	cache.startIndexScan()
	select {
	case <-cache.scanDone:
	case <-time.After(3 * time.Second):
		t.Fatal("启动扫描超时")
	}

	bytes, ready := cache.indexUsage()
	if !ready {
		t.Fatal("扫描完成后索引未就绪")
	}
	if want := completeBytes + 256; bytes != want {
		t.Fatalf("扫描重建的占用 = %d，期望 %d（两条完整 + 新残件 256B）", bytes, want)
	}
	cache.indexMu.Lock()
	entries := len(cache.index)
	cache.indexMu.Unlock()
	if entries != 3 {
		t.Fatalf("索引条目数 = %d，期望 3", entries)
	}
	if _, err := os.Stat(stalePaths.body); !os.IsNotExist(err) {
		t.Fatal("只剩 body 的老残件没有被清理")
	}
	if _, err := os.Stat(metaOnly.meta); !os.IsNotExist(err) {
		t.Fatal("只剩 meta 的老残件没有被清理")
	}
	if _, err := os.Stat(freshPaths.body); err != nil {
		t.Fatal("新残件被误删了（可能是正在写入的条目）")
	}
	if !imageCacheEntryOnDisk(cache, "a") || !imageCacheEntryOnDisk(cache, "b") {
		t.Fatal("完整条目被扫描误删")
	}
	if !readImageCacheEntry(t, cache, "a") {
		t.Fatal("扫描后 a 读不到")
	}
}

// 索引没建好之前不淘汰：宁可暂时超上限，也不能凭半个索引误删。
func TestImageCacheDoesNotEvictBeforeIndexReady(t *testing.T) {
	body := make([]byte, 2048)
	entrySize := measureImageCacheEntrySize(t, body)
	cache := newImageDiskCache(t.TempDir(), time.Hour, 2*entrySize)
	for i := 0; i < 6; i++ {
		storeImageCacheEntry(t, cache, fmt.Sprintf("k%d", i), body)
	}
	cache.evictOverflow() // 索引未就绪，应当直接返回
	for i := 0; i < 6; i++ {
		if !imageCacheEntryOnDisk(cache, fmt.Sprintf("k%d", i)) {
			t.Fatalf("索引就绪前淘汰了 k%d", i)
		}
	}
}

// 上限为 0（不限制）时，索引和淘汰全部关闭，行为与加容量上限之前一致。
func TestImageCacheUnlimitedKeepsEverything(t *testing.T) {
	body := make([]byte, 4096)
	cache := newImageDiskCache(t.TempDir(), time.Hour, 0)
	cache.scanIndex()
	for i := 0; i < 12; i++ {
		storeImageCacheEntry(t, cache, fmt.Sprintf("k%d", i), body)
	}
	cache.evictOverflow()
	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("k%d", i)
		if !imageCacheEntryOnDisk(cache, name) {
			t.Fatalf("不限容量时 %s 被淘汰了", name)
		}
		if !readImageCacheEntry(t, cache, name) {
			t.Fatalf("不限容量时 %s 读不到", name)
		}
	}
	bytes, ready := cache.indexUsage()
	if bytes != 0 || ready {
		t.Fatalf("不限容量时索引应保持空闲，实际 = (%d, ready=%v)", bytes, ready)
	}
	cache.indexMu.Lock()
	entries := len(cache.index)
	cache.indexMu.Unlock()
	if entries != 0 {
		t.Fatalf("不限容量时索引仍记录了 %d 条", entries)
	}
	stats, err := cache.Stats(true)
	if err != nil {
		t.Fatal(err)
	}
	if stats.MaxBytes != 0 || stats.Usage != 0 {
		t.Fatalf("不限容量时 stats = %+v，期望 maxBytes/usage 为 0", stats)
	}
}

func TestImageCacheStatsReportsCapacityUsage(t *testing.T) {
	body := make([]byte, 4096)
	entrySize := measureImageCacheEntrySize(t, body)
	cache := newImageDiskCache(t.TempDir(), time.Hour, 10*entrySize)
	cache.scanIndex()
	storeImageCacheEntry(t, cache, "a", body)
	storeImageCacheEntry(t, cache, "b", body)

	stats, err := cache.Stats(true)
	if err != nil {
		t.Fatal(err)
	}
	if stats.MaxBytes != 10*entrySize {
		t.Fatalf("stats.MaxBytes = %d，期望 %d", stats.MaxBytes, 10*entrySize)
	}
	if !stats.IndexReady {
		t.Fatal("stats.IndexReady = false，期望 true")
	}
	want := float64(stats.Bytes) / float64(stats.MaxBytes)
	if stats.Usage != want || stats.Usage <= 0 || stats.Usage >= 1 {
		t.Fatalf("stats.Usage = %v，期望 %v（0~1 之间）", stats.Usage, want)
	}
}

// Clear 之后索引要跟着清零，否则后续会按幻影占用误淘汰。
func TestImageCacheClearResetsIndex(t *testing.T) {
	body := make([]byte, 2048)
	cache := newImageDiskCache(t.TempDir(), time.Hour, 1<<20)
	cache.scanIndex()
	storeImageCacheEntry(t, cache, "a", body)
	if bytes, _ := cache.indexUsage(); bytes == 0 {
		t.Fatal("写入后索引占用仍为 0")
	}
	if err := cache.Clear(); err != nil {
		t.Fatal(err)
	}
	if bytes, _ := cache.indexUsage(); bytes != 0 {
		t.Fatalf("Clear 后索引占用 = %d，期望 0", bytes)
	}
}

// 并发读写：索引不能算错，也不能有数据竞争（go test -race）。
func TestImageCacheConcurrentReadWriteKeepsIndexConsistent(t *testing.T) {
	body := make([]byte, 1024)
	entrySize := measureImageCacheEntrySize(t, body)
	maxBytes := 16 * entrySize
	cache := newImageDiskCache(t.TempDir(), time.Hour, maxBytes)
	cache.scanIndex()

	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				name := fmt.Sprintf("w%d-%d", worker, i)
				res := bytesResponse(http.StatusOK, body, http.Header{"Content-Type": []string{"image/jpeg"}})
				if cache.wrapStore(httptestRequest(http.MethodGet), imageCacheTestKey(name), res, res.Header) {
					_, _ = io.Copy(io.Discard, res.Body)
					_ = res.Body.Close()
				}
				if cached, ok := cache.get(httptestRequest(http.MethodGet), imageCacheTestKey(name), "", config.ProxyEnv{}); ok {
					_, _ = io.Copy(io.Discard, cached.Body)
					_ = cached.Body.Close()
				}
				cache.Stats(true) //nolint:errcheck // 并发下只关心不炸
			}
		}(worker)
	}
	wg.Wait()

	// 后台淘汰跑完后，索引占用要收敛到上限以内
	// （淘汰只在「严格超过上限」时触发，所以这里断言的是上限而不是低水位；
	//  低水位由 TestImageCacheEvictsDownToLowWatermark 覆盖）。
	waitIndexBytes(t, cache, maxBytes)

	cache.indexMu.Lock()
	var sum int64
	for _, entry := range cache.index {
		sum += entry.size
	}
	total := cache.indexBytes
	cache.indexMu.Unlock()
	if sum != total {
		t.Fatalf("索引占用记账不一致：条目之和 %d，总数 %d", sum, total)
	}
	if total < 0 || total > maxBytes {
		t.Fatalf("索引占用 %d 超出上限 %d", total, maxBytes)
	}
}

// 命中路径不能碰磁盘：不 Chtimes、不重写 meta、不产生新文件，
// 访问时间只落在内存索引里。
func TestImageCacheHitDoesNotWriteToDisk(t *testing.T) {
	body := make([]byte, 8192)
	cache := newImageDiskCache(t.TempDir(), time.Hour, 1<<30)
	cache.scanIndex()
	storeImageCacheEntry(t, cache, "a", body)

	paths := cache.paths(imageCacheTestKey("a"))
	before := map[string]os.FileInfo{}
	for _, path := range []string{paths.body, paths.meta} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = info
	}
	filesBefore := imageCacheFileCount(t, cache.dir)

	cache.indexMu.Lock()
	firstAccess := cache.index[paths.hash].lastAccess
	cache.indexMu.Unlock()

	time.Sleep(2 * time.Millisecond)
	for i := 0; i < 50; i++ {
		if !readImageCacheEntry(t, cache, "a") {
			t.Fatalf("第 %d 次读取未命中", i)
		}
	}

	for path, info := range before {
		now, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !now.ModTime().Equal(info.ModTime()) {
			t.Fatalf("%s 的 mtime 被命中路径改写了：%v -> %v", filepath.Base(path), info.ModTime(), now.ModTime())
		}
		if now.Size() != info.Size() {
			t.Fatalf("%s 的大小被命中路径改写了", filepath.Base(path))
		}
	}
	if after := imageCacheFileCount(t, cache.dir); after != filesBefore {
		t.Fatalf("命中路径产生了新文件：%d -> %d", filesBefore, after)
	}

	cache.indexMu.Lock()
	lastAccess := cache.index[paths.hash].lastAccess
	cache.indexMu.Unlock()
	if lastAccess <= firstAccess {
		t.Fatal("命中没有更新内存里的访问时间")
	}
}

func imageCacheFileCount(t *testing.T, dir string) int {
	t.Helper()
	count := 0
	if err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d != nil && !d.IsDir() {
			count++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return count
}

// benchmarkImageCacheHit 对比「不限容量（无 LRU）」与「有容量上限（走 LRU 索引）」
// 两种配置下的缓存命中开销。
func benchmarkImageCacheHit(b *testing.B, maxBytes int64) {
	b.Helper()
	body := make([]byte, 16*1024)
	cache := newImageDiskCache(b.TempDir(), time.Hour, maxBytes)
	fixed := time.Unix(1_800_000_000, 0)
	cache.now = func() time.Time { return fixed }
	cache.scanIndex()

	key := imageCacheTestKey("bench")
	res := bytesResponse(http.StatusOK, body, http.Header{"Content-Type": []string{"image/jpeg"}})
	if !cache.wrapStore(httptestRequest(http.MethodGet), key, res, res.Header) {
		b.Fatal("wrapStore 没有接管响应体")
	}
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		b.Fatal(err)
	}
	_ = res.Body.Close()

	req := httptestRequest(http.MethodGet)
	buf := make([]byte, 32*1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cached, ok := cache.get(req, key, "", config.ProxyEnv{})
		if !ok {
			b.Fatal("缓存未命中")
		}
		if _, err := io.CopyBuffer(io.Discard, cached.Body, buf); err != nil {
			b.Fatal(err)
		}
		_ = cached.Body.Close()
	}
}

func BenchmarkImageCacheHitUnlimited(b *testing.B) { benchmarkImageCacheHit(b, 0) }

func BenchmarkImageCacheHitCapped(b *testing.B) { benchmarkImageCacheHit(b, 2<<30) }

func BenchmarkImageCacheIndexTouch(b *testing.B) {
	cache := newImageDiskCache(b.TempDir(), time.Hour, 2<<30)
	cache.scanIndex()
	cache.indexPut("deadbeef", 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cache.indexTouch("deadbeef")
	}
}

func BenchmarkImageCacheHitParallelCapped(b *testing.B) {
	benchmarkImageCacheHitParallel(b, 2<<30)
}

func BenchmarkImageCacheHitParallelUnlimited(b *testing.B) {
	benchmarkImageCacheHitParallel(b, 0)
}

// benchmarkImageCacheHitParallel 模拟 Emby 刷媒体库时的高并发图片命中，
// 用来验证 LRU 索引的锁不会成为热点。
func benchmarkImageCacheHitParallel(b *testing.B, maxBytes int64) {
	b.Helper()
	body := make([]byte, 16*1024)
	cache := newImageDiskCache(b.TempDir(), time.Hour, maxBytes)
	fixed := time.Unix(1_800_000_000, 0)
	cache.now = func() time.Time { return fixed }
	cache.scanIndex()

	const keys = 64
	for i := 0; i < keys; i++ {
		key := imageCacheTestKey(fmt.Sprintf("bench%d", i))
		res := bytesResponse(http.StatusOK, body, http.Header{"Content-Type": []string{"image/jpeg"}})
		if !cache.wrapStore(httptestRequest(http.MethodGet), key, res, res.Header) {
			b.Fatal("wrapStore 没有接管响应体")
		}
		if _, err := io.Copy(io.Discard, res.Body); err != nil {
			b.Fatal(err)
		}
		_ = res.Body.Close()
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		req := httptestRequest(http.MethodGet)
		buf := make([]byte, 32*1024)
		i := 0
		for pb.Next() {
			key := imageCacheTestKey(fmt.Sprintf("bench%d", i%keys))
			i++
			cached, ok := cache.get(req, key, "", config.ProxyEnv{})
			if !ok {
				b.Error("缓存未命中")
				return
			}
			if _, err := io.CopyBuffer(io.Discard, cached.Body, buf); err != nil {
				b.Error(err)
				return
			}
			_ = cached.Body.Close()
		}
	})
}

// 用 /proc/self/io 量化命中路径的磁盘 IO：不管有没有容量上限，
// 每次命中的写系统调用和写字节都必须是 0，读的量也不能变。
func TestImageCacheHitIssuesNoExtraDiskIO(t *testing.T) {
	const hits = 2000
	type sample struct {
		name     string
		readsPer float64
		writes   int64
		wchar    int64
	}
	samples := make([]sample, 0, 2)
	for _, tc := range []struct {
		name     string
		maxBytes int64
	}{
		{"不限容量（加 LRU 之前的行为）", 0},
		{"有容量上限（走 LRU 索引）", 2 << 30},
	} {
		body := make([]byte, 16*1024)
		cache := newImageDiskCache(t.TempDir(), time.Hour, tc.maxBytes)
		fixed := time.Unix(1_800_000_000, 0)
		cache.now = func() time.Time { return fixed }
		cache.scanIndex()
		storeImageCacheEntry(t, cache, "io", body)

		req := httptestRequest(http.MethodGet)
		buf := make([]byte, 32*1024)
		key := imageCacheTestKey("io")
		for i := 0; i < 50; i++ { // 预热：让 meta 进内存缓存
			readImageCacheEntry(t, cache, "io")
		}

		before, ok := readProcSelfIO(t)
		if !ok {
			t.Skip("当前平台没有 /proc/self/io，跳过 IO 计数")
		}
		start := time.Now()
		for i := 0; i < hits; i++ {
			cached, hit := cache.get(req, key, "", config.ProxyEnv{})
			if !hit {
				t.Fatalf("第 %d 次命中失败", i)
			}
			if _, err := io.CopyBuffer(io.Discard, cached.Body, buf); err != nil {
				t.Fatal(err)
			}
			_ = cached.Body.Close()
		}
		elapsed := time.Since(start)
		after, _ := readProcSelfIO(t)

		s := sample{
			name:     tc.name,
			readsPer: float64(after["syscr"]-before["syscr"]) / hits,
			writes:   after["syscw"] - before["syscw"],
			wchar:    after["wchar"] - before["wchar"],
		}
		samples = append(samples, s)
		t.Logf("%s：%d 次命中 %.0f ns/次，读系统调用 %.2f 次/命中，写系统调用 %d，写字节 %d",
			tc.name, hits, float64(elapsed.Nanoseconds())/hits, s.readsPer, s.writes, s.wchar)
	}

	// /proc/self/io 是进程级计数，同包其它测试的后台 goroutine（日志落盘等）
	// 会掺进来，所以这里留了很宽的余量：真在命中时写 meta / Chtimes 的话，
	// 写次数会是每次命中至少一次（即 hits 次），远超这个阈值。
	for _, s := range samples {
		if s.writes > int64(hits)/4 {
			t.Fatalf("%s 的命中路径产生了写 IO：syscw=%d wchar=%d（%d 次命中）", s.name, s.writes, s.wchar, hits)
		}
	}
	if delta := samples[1].readsPer - samples[0].readsPer; delta > 0.05 {
		t.Fatalf("加了 LRU 之后每次命中多出 %.2f 次读系统调用", delta)
	}
}

func readProcSelfIO(t *testing.T) (map[string]int64, bool) {
	t.Helper()
	data, err := os.ReadFile("/proc/self/io")
	if err != nil {
		return nil, false
	}
	out := map[string]int64{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		n, convErr := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if convErr != nil {
			continue
		}
		out[strings.TrimSpace(key)] = n
	}
	return out, len(out) > 0
}
