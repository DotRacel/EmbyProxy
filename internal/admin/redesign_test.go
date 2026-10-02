package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"embyproxy/internal/auth"
	"embyproxy/internal/config"
	"embyproxy/internal/logging"
	"embyproxy/internal/probe"
	"embyproxy/internal/storage"
	"embyproxy/internal/telegram"
)

func newAssetTestHandler(t *testing.T) *Handler {
	t.Helper()
	// 不配置 ADMIN_TOKEN、也不挂 checker：静态文件既不需要登录，也不受 Token 配置错误影响。
	handler := New(config.Config{}, nil, nil, nil, nil, nil)
	handler.assets = fstest.MapFS{
		"app.css":            {Data: []byte("body{color:red}")},
		"core.js":            {Data: []byte("console.log(1)")},
		"fonts/inter.woff2":  {Data: []byte("wOF2")},
		"icons/logo.svg":     {Data: []byte("<svg/>")},
		"img/shot.png":       {Data: []byte("\x89PNG")},
		"fonts/nested/x.bin": {Data: []byte("x")},
	}
	return handler
}

func serveAsset(handler *Handler, method, path string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for key, values := range header {
		req.Header[key] = values
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestServeAssetsWithTypesAndRevalidation(t *testing.T) {
	handler := newAssetTestHandler(t)
	cases := map[string]string{
		"/admin/assets/app.css":           "text/css; charset=utf-8",
		"/admin/assets/core.js":           "text/javascript; charset=utf-8",
		"/admin/assets/fonts/inter.woff2": "font/woff2",
		"/admin/assets/icons/logo.svg":    "image/svg+xml",
		"/admin/assets/img/shot.png":      "image/png",
	}
	for path, wantType := range cases {
		rec := serveAsset(handler, http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d", path, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != wantType {
			t.Fatalf("GET %s Content-Type = %q, want %q", path, got, wantType)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("GET %s Cache-Control = %q", path, got)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("GET %s missing nosniff", path)
		}
	}

	first := serveAsset(handler, http.MethodGet, "/admin/assets/app.css", nil)
	etag := first.Header().Get("ETag")
	if !strings.HasPrefix(etag, `"`) || len(etag) < 10 || first.Body.String() != "body{color:red}" {
		t.Fatalf("etag = %q body = %q", etag, first.Body.String())
	}
	if other := serveAsset(handler, http.MethodGet, "/admin/assets/core.js", nil).Header().Get("ETag"); other == etag {
		t.Fatal("different files must not share an ETag")
	}
	cached := serveAsset(handler, http.MethodGet, "/admin/assets/app.css", http.Header{"If-None-Match": {etag}})
	if cached.Code != http.StatusNotModified {
		t.Fatalf("revalidation status = %d, want 304", cached.Code)
	}
	head := serveAsset(handler, http.MethodHead, "/admin/assets/app.css", nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD status = %d body = %q", head.Code, head.Body.String())
	}
	if post := serveAsset(handler, http.MethodPost, "/admin/assets/app.css", nil); post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", post.Code)
	}
}

func TestServeAssetsRejectsMissingDirectoriesAndTraversal(t *testing.T) {
	handler := newAssetTestHandler(t)
	for _, path := range []string{
		"/admin/assets/",
		"/admin/assets/missing.css",
		"/admin/assets/fonts",
		"/admin/assets/fonts/",
		"/admin/assets/../index.html",
		"/admin/assets/fonts/../../index.html",
		"/admin/assets//app.css",
		"/admin/assets/./app.css",
		"/admin/assets/%2e%2e/index.html",
	} {
		if rec := serveAsset(handler, http.MethodGet, path, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s status = %d, want 404", path, rec.Code)
		}
	}
}

func TestEmbeddedStaticServesIndexAndAssets(t *testing.T) {
	cfg := config.Config{AdminToken: "strong-random-admin-token"}
	handler := New(cfg, nil, auth.NewChecker(cfg, nil), nil, nil, nil)

	rec := serveAsset(handler, http.MethodGet, "/admin", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != indexHTML {
		t.Fatalf("GET /admin status = %d", rec.Code)
	}
	if rec.Header().Get("ETag") != indexETag || rec.Header().Get("Cache-Control") != "no-cache, must-revalidate" {
		t.Fatalf("index headers = %v", rec.Header())
	}
	if !strings.HasPrefix(adminSource, indexHTML) {
		t.Fatal("adminSource must start with index.html")
	}

	// 真正嵌入的 assets：存在什么就验证什么，不依赖前端具体文件名。
	entries, err := staticFS.ReadDir("static/assets")
	if err != nil {
		t.Skip("static/assets not present yet")
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := "/admin/assets/" + entry.Name()
		res := serveAsset(handler, http.MethodGet, path, nil)
		if res.Code != http.StatusOK || res.Header().Get("ETag") == "" {
			t.Fatalf("GET %s status = %d etag = %q", path, res.Code, res.Header().Get("ETag"))
		}
		if strings.HasSuffix(entry.Name(), ".css") || strings.HasSuffix(entry.Name(), ".js") {
			if !strings.Contains(adminSource, res.Body.String()) {
				t.Fatalf("adminSource missing %s", entry.Name())
			}
		}
	}
}

func TestListAndExportDropKeepaliveFields(t *testing.T) {
	ctx := context.Background()
	handler, closeStore := newConfigTestHandler(t)
	defer closeStore()
	res := handler.save(ctx, "admin", map[string]any{"node": map[string]any{
		"name": "alpha", "target": "https://a.example.com",
		// 老前端还会带这些字段，应被忽略而不是报错。
		"renewDays": 30, "remindBeforeDays": 3, "keepaliveAt": "03:00", "keepaliveMaxPerDay": 2, "keepaliveChangeOnly": false,
	}})
	if res["ok"] != true {
		t.Fatalf("save() = %+v", res)
	}
	for _, payload := range []any{handler.list(ctx, "admin"), handler.exportNodes(ctx, "admin", nil)} {
		raw, _ := json.Marshal(payload)
		for _, key := range []string{"renewDays", "remindBeforeDays", "keepaliveAt", "keepaliveMaxPerDay", "keepaliveChangeOnly", "lastPlayAt"} {
			if strings.Contains(string(raw), `"`+key+`"`) {
				t.Fatalf("response still contains %s: %s", key, raw)
			}
		}
	}
	if res, _ := handler.dispatch(ctx, "admin", "keepalive.test", map[string]any{}); res["ok"] != false || !strings.Contains(asString(res["error"]), "未知 action") {
		t.Fatalf("keepalive.test = %+v, want unknown action", res)
	}
}

func TestTGSetAlertFields(t *testing.T) {
	ctx := context.Background()
	handler, closeStore := newConfigTestHandler(t)
	defer closeStore()

	res := handler.tgSet(ctx, map[string]any{"config": map[string]any{
		"enabled": true, "token": "t", "chat": "c",
		"alertNodes": false, "alertErrors": true, "alertFailThreshold": 5,
	}})
	if res["ok"] != true {
		t.Fatalf("tgSet() = %+v", res)
	}
	cfg, _ := handler.store.GetTGConfig(ctx)
	if cfg.AlertNodes || !cfg.AlertErrors || cfg.AlertFailThreshold != 5 {
		t.Fatalf("saved config = %+v", cfg)
	}

	// 老版本面板不带告警字段：沿用已保存的值，不能悄悄关掉或重置。
	if res := handler.tgSet(ctx, map[string]any{"config": map[string]any{"enabled": true, "token": "t", "chat": "c"}}); res["ok"] != true {
		t.Fatalf("tgSet() = %+v", res)
	}
	cfg, _ = handler.store.GetTGConfig(ctx)
	if cfg.AlertNodes || !cfg.AlertErrors || cfg.AlertFailThreshold != 5 {
		t.Fatalf("config after legacy save = %+v", cfg)
	}

	for _, bad := range []any{0, 11, "abc", 2.5, ""} {
		res := handler.tgSet(ctx, map[string]any{"config": map[string]any{"alertFailThreshold": bad}})
		if res["ok"] == true || !strings.Contains(asString(res["error"]), "节点故障判定的连续失败次数") {
			t.Fatalf("tgSet(threshold=%v) = %+v, want Chinese range error", bad, res)
		}
	}

	got, _ := handler.dispatch(ctx, "admin", "tg.get", nil)
	raw, _ := json.Marshal(got["config"])
	for _, key := range []string{`"alertNodes":false`, `"alertErrors":true`, `"alertFailThreshold":5`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("tg.get config = %s, want %s", raw, key)
		}
	}
}

type tgRoundTrip func(*http.Request) (*http.Response, error)

func (f tgRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestTGTestSendsChannelCheckOrReport(t *testing.T) {
	ctx := context.Background()
	handler, closeStore := newConfigTestHandler(t)
	defer closeStore()
	type sent struct{ token, chat, text string }
	var calls []sent
	tg := telegram.New(handler.store, logging.New("silent", false))
	tg.SetHTTPClient(&http.Client{Transport: tgRoundTrip(func(req *http.Request) (*http.Response, error) {
		var body struct {
			Chat string `json:"chat_id"`
			Text string `json:"text"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		token := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/bot"), "/sendMessage")
		calls = append(calls, sent{token: token, chat: body.Chat, text: body.Text})
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})})
	handler.telegram = tg

	if res, _ := handler.dispatch(ctx, "admin", "tg.test", map[string]any{}); res["ok"] != false || res["error"] != "TG 未配置" {
		t.Fatalf("tg.test without config = %+v", res)
	}

	// 表单值优先：还没保存的 Token / Chat ID 也能试发，备注照常带上。
	override := map[string]any{"enabled": true, "token": "form-token", "chat": "form-chat", "serverRemark": "家庭服"}
	if res, _ := handler.dispatch(ctx, "admin", "tg.test", map[string]any{"tg": override}); res["ok"] != true {
		t.Fatalf("tg.test = %+v", res)
	}
	if len(calls) != 1 || calls[0].token != "form-token" || calls[0].chat != "form-chat" {
		t.Fatalf("calls = %+v", calls)
	}
	if want := "🏷️ 服务器：家庭服\n\n" + telegram.TestMessage; calls[0].text != want {
		t.Fatalf("test text = %q, want %q", calls[0].text, want)
	}
	if cfg, _ := handler.store.GetTGConfig(ctx); cfg.Token != "" {
		t.Fatal("tg.test must not save the override")
	}

	if res, _ := handler.dispatch(ctx, "admin", "tg.test", map[string]any{"tg": override, "kind": "report"}); res["ok"] != true {
		t.Fatalf("tg.test report = %+v", res)
	}
	if len(calls) != 2 || !strings.Contains(calls[1].text, "📊 Emby 播放日报") {
		t.Fatalf("report call = %+v", calls)
	}

	// 没带 tg 时用已保存的配置。
	if res := handler.tgSet(ctx, map[string]any{"config": map[string]any{"enabled": true, "token": "saved", "chat": "saved-chat"}}); res["ok"] != true {
		t.Fatalf("tgSet() = %+v", res)
	}
	if res, _ := handler.dispatch(ctx, "admin", "tg.test", map[string]any{}); res["ok"] != true || calls[2].token != "saved" {
		t.Fatalf("tg.test saved = %+v calls = %+v", res, calls)
	}

	if res, _ := handler.dispatch(ctx, "admin", "tg.test", map[string]any{"tg": map[string]any{"token": "x", "chat": "y", "alertFailThreshold": 0}}); res["ok"] != false {
		t.Fatalf("tg.test with invalid override = %+v", res)
	}
}

func TestProbeSeriesPerTargetAndSummaryActiveTarget(t *testing.T) {
	ctx := context.Background()
	handler, closeStore := newConfigTestHandler(t)
	defer closeStore()
	if err := handler.store.SaveNode(ctx, "admin", storage.Node{Name: "alpha", Target: "https://a.example.com\nhttps://b.example.com"}); err != nil {
		t.Fatal(err)
	}
	reg := probe.NewRegistry()
	now := time.Now().UnixMilli()
	reg.RecordNode("alpha", probe.Sample{At: now - 1000, MS: 90, OK: true})
	reg.RecordTarget("alpha", "https://a.example.com", probe.Sample{At: now - 1000, Err: "超时"})
	reg.RecordTarget("alpha", "https://b.example.com", probe.Sample{At: now - 1000, MS: 90, OK: true})
	handler.AttachProbes(reg, nil)
	handler.AttachActiveTargets(func(uid, name string, targets []string) string {
		if uid == "admin" && name == "alpha" && len(targets) == 2 {
			return targets[1]
		}
		return ""
	})

	plain := handler.probeSeries(ctx, "admin", map[string]any{"name": "alpha", "hours": 1})
	if _, has := plain["targets"]; has {
		t.Fatal("targets must only be returned when perTarget is set")
	}
	res := handler.probeSeries(ctx, "admin", map[string]any{"name": "alpha", "hours": 1, "perTarget": true})
	raw, _ := json.Marshal(res)
	var decoded struct {
		Points  []probe.Point `json:"points"`
		Targets []struct {
			Target  string        `json:"target"`
			Primary bool          `json:"primary"`
			Points  []probe.Point `json:"points"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Points) == 0 || len(decoded.Targets) != 2 {
		t.Fatalf("series = %s", raw)
	}
	if decoded.Targets[0].Target != "https://a.example.com" || !decoded.Targets[0].Primary || decoded.Targets[1].Primary {
		t.Fatalf("targets order = %+v", decoded.Targets)
	}
	last := decoded.Targets[0].Points[len(decoded.Targets[0].Points)-1]
	if last.N != 1 || last.Err != "超时" {
		t.Fatalf("primary last point = %+v", last)
	}
	if !strings.Contains(string(raw), `"n":0`) {
		t.Fatalf("empty buckets must still carry n: %s", raw)
	}

	summary := handler.probeSummary(ctx, "admin")
	stats := summary["probes"].([]probe.NodeStats)
	if len(stats) != 1 || stats[0].ActiveTarget != "https://b.example.com" {
		t.Fatalf("summary = %+v", stats)
	}
	sraw, _ := json.Marshal(stats[0])
	for _, key := range []string{`"activeTarget":"https://b.example.com"`, `"availability":0`, `"okSamples":1`, `"avgMs":90`} {
		if !strings.Contains(string(sraw), key) {
			t.Fatalf("summary json = %s, want %s", sraw, key)
		}
	}
}
