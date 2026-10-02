package proxy

import (
	"context"
	"errors"
	"testing"

	"embyproxy/internal/storage"
)

func TestActiveTargetReportsLineChosenByFailover(t *testing.T) {
	h, node, _ := newLineBanTestHandler(t, func(host string) (int, error) {
		if host == "a.example" {
			return 0, errors.New("connection refused")
		}
		return 200, nil
	})
	targets := storage.SplitTargets(node.Target)

	// 还没有请求经过时不猜测，留空让面板显示「未知」。
	if got := h.ActiveTarget("admin", "node", targets); got != "" {
		t.Fatalf("ActiveTarget before any request = %q, want empty", got)
	}

	lineBanTestRequest(t, h, context.Background(), node)
	if got := h.ActiveTarget("admin", "NODE", targets); got != lineBanTestB {
		t.Fatalf("ActiveTarget = %q, want failover target %q", got, lineBanTestB)
	}
	// 记录的线路已经不在配置里（比如刚改过上游）时同样留空。
	if got := h.ActiveTarget("admin", "node", []string{lineBanTestA}); got != "" {
		t.Fatalf("ActiveTarget for stale target = %q, want empty", got)
	}

	h.ResetNodeRoutingState("admin", "node")
	if got := h.ActiveTarget("admin", "node", targets); got != "" {
		t.Fatalf("ActiveTarget after reset = %q, want empty", got)
	}
}
