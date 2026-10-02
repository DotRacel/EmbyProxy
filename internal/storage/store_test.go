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

// 保号提醒移除后，老数据里的 xd/xb/xh/xk/xco 必须被静默忽略，其余字段照常解出，
// 重新打包时这些键随之消失。
func TestUnpackNodeIgnoresLegacyKeepaliveKeys(t *testing.T) {
	node, ok := UnpackNode("alpha", `{"t":"http://a","d":"Alpha","xd":30,"xb":3,"xh":"03:00","xk":2,"xco":0,"im":0}`)
	if !ok {
		t.Fatalf("UnpackNode 失败")
	}
	if node.Target != "http://a" || node.DisplayName != "Alpha" || node.Impersonate {
		t.Fatalf("老数据其余字段应保持不变: %+v", node)
	}
	packed, err := PackNode(node)
	if err != nil {
		t.Fatalf("PackNode: %v", err)
	}
	for _, key := range []string{`"xd"`, `"xb"`, `"xh"`, `"xk"`, `"xco"`} {
		if strings.Contains(packed, key) {
			t.Fatalf("重新打包后不应再写入 %s: %s", key, packed)
		}
	}
}

func TestInitSchemaDropsKeepaliveLeftovers(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	store, err := New(path)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	// 模拟老版本留下的表和按天写的提醒标记。
	if _, err := store.DB().ExecContext(ctx, `CREATE TABLE keepalive_state (node TEXT PRIMARY KEY, anchor_ts INTEGER NOT NULL)`); err != nil {
		t.Fatalf("create keepalive_state: %v", err)
	}
	for _, key := range []string{"keepalive:last:admin:alpha:2026-01-01", "keepalive:digest:admin:alpha:2026-01-01", "report:cnt:2026-01-01"} {
		if err := store.KV().Put(ctx, key, "1"); err != nil {
			t.Fatalf("Put(%q) error = %v", key, err)
		}
	}
	_ = store.Close()

	store, err = New(path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var count int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'keepalive_state'`).Scan(&count); err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	if count != 0 {
		t.Fatal("keepalive_state should be dropped")
	}
	res, err := store.KV().List(ctx, "keepalive:", 0, 100)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(res.Keys) != 0 {
		t.Fatalf("keepalive KV keys left: %v", res.Keys)
	}
	if _, ok, _ := store.KV().Get(ctx, "report:cnt:2026-01-01"); !ok {
		t.Fatal("unrelated KV keys must survive the cleanup")
	}
}

func TestTGConfigDefaultsAlertsWhenFieldsAbsent(t *testing.T) {
	ctx := context.Background()
	store, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// 全新安装：没有任何通知配置。
	got, err := store.GetTGConfig(ctx)
	if err != nil {
		t.Fatalf("GetTGConfig() error = %v", err)
	}
	if got.Enabled || !got.AlertNodes || !got.AlertErrors || got.AlertFailThreshold != DefaultAlertFailThreshold {
		t.Fatalf("fresh config = %+v, want alerts on with default threshold", got)
	}

	// 老版本保存的配置里没有告警字段：缺省不等于 false。
	if err := store.KV().Put(ctx, "tg:config", map[string]any{"enabled": true, "token": "token", "chat": "chat", "reportEnabled": false}); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	got, err = store.GetTGConfig(ctx)
	if err != nil {
		t.Fatalf("GetTGConfig() error = %v", err)
	}
	if !got.AlertNodes || !got.AlertErrors || got.AlertFailThreshold != DefaultAlertFailThreshold {
		t.Fatalf("legacy config = %+v, want alerts on with default threshold", got)
	}
	if !got.NodeAlertsOn() || !got.ErrorAlertsOn() {
		t.Fatalf("legacy enabled config should send alerts: %+v", got)
	}

	// 显式关闭必须保留；超出范围的阈值回落到默认值。
	if err := store.KV().Put(ctx, "tg:config", map[string]any{"enabled": true, "alertNodes": false, "alertErrors": false, "alertFailThreshold": 99}); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	got, err = store.GetTGConfig(ctx)
	if err != nil {
		t.Fatalf("GetTGConfig() error = %v", err)
	}
	if got.AlertNodes || got.AlertErrors || got.AlertFailThreshold != DefaultAlertFailThreshold {
		t.Fatalf("explicit config = %+v, want alerts off and threshold reset to default", got)
	}

	cfg := DefaultTGConfig()
	cfg.AlertFailThreshold = 5
	if err := store.SaveTGConfig(ctx, cfg); err != nil {
		t.Fatalf("SaveTGConfig() error = %v", err)
	}
	if got, _ := store.GetTGConfig(ctx); got.AlertFailThreshold != 5 {
		t.Fatalf("AlertFailThreshold = %d, want 5", got.AlertFailThreshold)
	}
}

func TestNodeAlertStateLifecycle(t *testing.T) {
	ctx := context.Background()
	store, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.SaveNode(ctx, "admin", Node{Name: "alpha", Target: "http://a"}); err != nil {
		t.Fatalf("SaveNode() error = %v", err)
	}
	want := NodeAlertState{Since: 1000, DownAt: 4000}
	if err := store.SaveNodeAlertState(ctx, "admin", "Alpha", want); err != nil {
		t.Fatalf("SaveNodeAlertState() error = %v", err)
	}
	if got, ok, err := store.GetNodeAlertState(ctx, "admin", "alpha"); err != nil || !ok || got != want {
		t.Fatalf("GetNodeAlertState() = %+v, %v, %v", got, ok, err)
	}
	states, err := store.ListNodeAlertStates(ctx, "admin")
	if err != nil || len(states) != 1 || states["alpha"] != want {
		t.Fatalf("ListNodeAlertStates() = %+v, %v", states, err)
	}

	// 删除（以及改名时删除旧名）要一并清掉故障状态。
	if err := store.DeleteNode(ctx, "admin", "alpha"); err != nil {
		t.Fatalf("DeleteNode() error = %v", err)
	}
	if _, ok, err := store.GetNodeAlertState(ctx, "admin", "alpha"); err != nil || ok {
		t.Fatalf("alert state after delete: ok=%v err=%v", ok, err)
	}
}
