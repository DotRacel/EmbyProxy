package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newProbeTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := New(filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestProbeSamplesRoundTripAndOverwrite(t *testing.T) {
	ctx := context.Background()
	store := newProbeTestStore(t)
	now := time.Now().UnixMilli()

	if err := store.AppendProbeSamples(ctx, []ProbeSample{
		{Node: "uhdnow", Target: "", At: now - 2000, MS: 140, Status: 200, OK: true},
		{Node: "uhdnow", Target: "https://v1.example.com", At: now - 2000, MS: 140, Status: 200, OK: true},
		{Node: "uhdnow", Target: "https://v2.example.com", At: now - 2000, MS: 8000, Err: "超时"},
		{Node: "", At: now, MS: 1},   // 无节点名应被跳过
		{Node: "skip", At: 0, MS: 1}, // 无时间戳应被跳过
	}); err != nil {
		t.Fatal(err)
	}

	got, err := store.LoadProbeSamples(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("len(samples) = %d, want 3: %+v", len(got), got)
	}

	// 同一 (node, target, at) 重写应覆盖而不是报主键冲突。
	if err := store.AppendProbeSamples(ctx, []ProbeSample{
		{Node: "uhdnow", Target: "https://v2.example.com", At: now - 2000, MS: 220, Status: 200, OK: true},
	}); err != nil {
		t.Fatal(err)
	}
	got, err = store.LoadProbeSamples(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("len(samples) after overwrite = %d, want 3", len(got))
	}
	for _, s := range got {
		if s.Target != "https://v2.example.com" {
			continue
		}
		if s.MS != 220 || !s.OK || s.Err != "" {
			t.Fatalf("overwritten sample = %+v, want 220ms ok", s)
		}
	}
}

func TestLoadProbeSamplesRespectsWindowAndOrder(t *testing.T) {
	ctx := context.Background()
	store := newProbeTestStore(t)
	now := time.Now().UnixMilli()

	if err := store.AppendProbeSamples(ctx, []ProbeSample{
		{Node: "uhdnow", At: now - 3*time.Hour.Milliseconds(), MS: 500, OK: true},
		{Node: "uhdnow", At: now - 1000, MS: 200, OK: true},
		{Node: "uhdnow", At: now - 2000, MS: 300, OK: true},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := store.LoadProbeSamples(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len(samples) = %d, want 2 (窗口外的应被过滤)", len(got))
	}
	if got[0].At > got[1].At {
		t.Fatalf("samples not ascending: %+v", got)
	}
}

func TestPruneAndRetainProbeSamples(t *testing.T) {
	ctx := context.Background()
	store := newProbeTestStore(t)
	now := time.Now().UnixMilli()

	if err := store.AppendProbeSamples(ctx, []ProbeSample{
		{Node: "keep", At: now - 1000, MS: 100, OK: true},
		{Node: "keep", At: now - 30*time.Hour.Milliseconds(), MS: 100, OK: true},
		{Node: "gone", At: now - 1000, MS: 100, OK: true},
	}); err != nil {
		t.Fatal(err)
	}

	if err := store.PruneProbeSamples(ctx, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	got, err := store.LoadProbeSamples(ctx, 48*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len(samples) after prune = %d, want 2", len(got))
	}

	// names 为空更可能是一次读取失败，不该顺手清库。
	if err := store.RetainProbeSamples(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got, err = store.LoadProbeSamples(ctx, 48*time.Hour); err != nil || len(got) != 2 {
		t.Fatalf("retain(nil) removed rows: len = %d, err = %v", len(got), err)
	}

	if err := store.RetainProbeSamples(ctx, []string{"keep"}); err != nil {
		t.Fatal(err)
	}
	got, err = store.LoadProbeSamples(ctx, 48*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Node != "keep" {
		t.Fatalf("samples after retain = %+v, want only keep", got)
	}

	if err := store.DeleteProbeSamples(ctx, "keep"); err != nil {
		t.Fatal(err)
	}
	if got, err = store.LoadProbeSamples(ctx, 48*time.Hour); err != nil || len(got) != 0 {
		t.Fatalf("samples after delete = %+v, err = %v", got, err)
	}
}
