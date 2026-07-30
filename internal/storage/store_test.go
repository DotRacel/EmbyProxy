package storage

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdmin2FAConfigCRUDAndCorruption(t *testing.T) {
	ctx := context.Background()
	store, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if _, configured, err := store.GetAdmin2FAConfig(ctx); err != nil || configured {
		t.Fatalf("initial config = configured %v, err %v", configured, err)
	}
	want := Admin2FAConfig{Version: 1, Salt: "salt", Nonce: "nonce", Ciphertext: "cipher", EnrolledAt: 1234, LastUsedStep: 42}
	if err := store.SaveAdmin2FAConfig(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, configured, err := store.GetAdmin2FAConfig(ctx)
	if err != nil || !configured || got != want {
		t.Fatalf("stored config = %+v, configured %v, err %v", got, configured, err)
	}
	if err := store.KV().Put(ctx, admin2FAConfigKey, "{broken"); err != nil {
		t.Fatal(err)
	}
	if _, configured, err := store.GetAdmin2FAConfig(ctx); err == nil || !configured {
		t.Fatalf("corrupt config = configured %v, err %v", configured, err)
	}
	if err := store.DeleteAdmin2FAConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if _, configured, err := store.GetAdmin2FAConfig(ctx); err != nil || configured {
		t.Fatalf("deleted config = configured %v, err %v", configured, err)
	}
}

func TestDefaultSystemConfigDoesNotTrustProxyHeaders(t *testing.T) {
	if DefaultSystemConfig().TrustProxy {
		t.Fatal("TrustProxy default should be false")
	}
}

func TestSystemConfigBackfillsImageDefaults(t *testing.T) {
	ctx := context.Background()
	store, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	fallback := DefaultSystemConfig()
	if err := store.KV().Put(ctx, "system:config", map[string]any{"logLevel": "debug"}); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	got, err := store.GetSystemConfig(ctx, fallback)
	if err != nil {
		t.Fatalf("GetSystemConfig() error = %v", err)
	}
	if got.LogLevel != "debug" {
		t.Fatalf("LogLevel = %q, want debug", got.LogLevel)
	}
	if got.ImageProxyLimitEnabled != fallback.ImageProxyLimitEnabled || got.ImageProxyMaxConcurrent != fallback.ImageProxyMaxConcurrent || got.ImageProxyRequestIntervalMS != fallback.ImageProxyRequestIntervalMS || got.ImageCacheEnabled != fallback.ImageCacheEnabled || got.ImageCacheTTLDays != fallback.ImageCacheTTLDays {
		t.Fatalf("image settings = %+v, want defaults %+v", got, fallback)
	}
}

func TestSystemConfigCacheRefreshesOnSave(t *testing.T) {
	ctx := context.Background()
	store, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	fallback := DefaultSystemConfig()
	first, err := store.GetSystemConfig(ctx, fallback)
	if err != nil {
		t.Fatalf("GetSystemConfig() error = %v", err)
	}
	if first.LogLevel != fallback.LogLevel {
		t.Fatalf("LogLevel = %q; want fallback %q", first.LogLevel, fallback.LogLevel)
	}

	next := fallback
	next.LogLevel = "debug"
	next.TrustProxy = !fallback.TrustProxy
	if err := store.SaveSystemConfig(ctx, next); err != nil {
		t.Fatalf("SaveSystemConfig() error = %v", err)
	}

	got, err := store.GetSystemConfig(ctx, fallback)
	if err != nil {
		t.Fatalf("GetSystemConfig() after save error = %v", err)
	}
	if got.LogLevel != next.LogLevel || got.TrustProxy != next.TrustProxy {
		t.Fatalf("GetSystemConfig() = %+v; want saved %+v", got, next)
	}
}

func TestTGConfigBackfillsReportEnabledForLegacyEnabledConfig(t *testing.T) {
	ctx := context.Background()
	store, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	if err := store.KV().Put(ctx, "tg:config", map[string]any{
		"enabled": true,
		"token":   "token",
		"chat":    "chat",
	}); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	got, err := store.GetTGConfig(ctx)
	if err != nil {
		t.Fatalf("GetTGConfig() error = %v", err)
	}
	if !got.ReportEnabled {
		t.Fatalf("ReportEnabled = false, want true for legacy enabled config")
	}
	if got.ServerRemark != "" {
		t.Fatalf("ServerRemark = %q, want empty for legacy config", got.ServerRemark)
	}
}

func TestTGConfigKeepsExplicitReportDisabled(t *testing.T) {
	ctx := context.Background()
	store, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	if err := store.SaveTGConfig(ctx, TGConfig{
		Enabled:       true,
		Token:         "token",
		Chat:          "chat",
		ReportEnabled: false,
	}); err != nil {
		t.Fatalf("SaveTGConfig() error = %v", err)
	}

	got, err := store.GetTGConfig(ctx)
	if err != nil {
		t.Fatalf("GetTGConfig() error = %v", err)
	}
	if got.ReportEnabled {
		t.Fatalf("ReportEnabled = true, want false")
	}
}

// 手动排序移除后，老数据里残留的 "r" 键必须被静默忽略（节点整条打包成 JSON 存在
// proxy_kv 里，没有列要删），并且下一次保存时自然从存量里消失。
func TestUnpackNodeIgnoresLegacyRankKey(t *testing.T) {
	node, ok := UnpackNode("alpha", `{"t":"http://a","r":7,"f":1}`)
	if !ok {
		t.Fatalf("UnpackNode 失败")
	}
	if node.Target != "http://a" || !node.Fav {
		t.Fatalf("老数据其余字段应保持不变: %+v", node)
	}
	packed, err := PackNode(node)
	if err != nil {
		t.Fatalf("PackNode: %v", err)
	}
	if strings.Contains(packed, `"r"`) {
		t.Fatalf("重新打包后不应再写入 rank: %s", packed)
	}
}

// 去掉 Rank 之后的兜底顺序：收藏优先，其余按名称升序，反复排序结果必须一致。
func TestSortNodesFavoritesFirstThenName(t *testing.T) {
	nodes := []Node{
		{Name: "delta"},
		{Name: "alpha"},
		{Name: "zeta", Fav: true},
		{Name: "beta", Fav: true},
		{Name: "charlie"},
	}
	SortNodes(nodes)
	want := []string{"beta", "zeta", "alpha", "charlie", "delta"}
	for i, name := range want {
		if nodes[i].Name != name {
			t.Fatalf("第 %d 个节点应为 %s，实际 %s（完整顺序 %+v）", i, name, nodes[i].Name, nodes)
		}
	}
	SortNodes(nodes)
	for i, name := range want {
		if nodes[i].Name != name {
			t.Fatalf("再次排序后顺序发生变化: %+v", nodes)
		}
	}
}
