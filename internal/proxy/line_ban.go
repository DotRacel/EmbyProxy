package proxy

import (
	"context"
	"errors"
	"time"
)

const (
	// lineBanStrikes is how many line failures within lineStrikeWindow it takes
	// to bench a target. A single failed request says little about a line - an
	// endpoint-specific 5xx or one reset is common - and benching on it bounces
	// every other viewer of the node onto a different line for a minute.
	lineBanStrikes   = 3
	lineStrikeWindow = time.Minute
	lineBanDuration  = time.Minute
)

func lineBanKey(nodeKey, target string) string {
	return nodeKey + "|" + target
}

// noteLineFailure records a failure the line itself is responsible for and
// benches the line once they pile up. Only report transport errors while the
// client is still waiting (isLineTransportFailure) and 5xx answers: a 403/404
// is about the requested path, and a canceled client says nothing about the
// upstream.
func (h *Handler) noteLineFailure(banKey string) {
	if h.lineStrikes.Incr(banKey, lineStrikeWindow) >= lineBanStrikes {
		h.lineStrikes.Delete(banKey)
		h.lineBan.Set(banKey, 1, lineBanDuration)
	}
}

// noteLineSuccess clears the strikes and any ban on a line that just answered,
// since that answer is better evidence of its health than the earlier failures.
func (h *Handler) noteLineSuccess(banKey string) {
	h.lineStrikes.Delete(banKey)
	h.lineBan.Delete(banKey)
}

func isLineTransportFailure(ctx context.Context, err error) bool {
	return err != nil && ctx.Err() == nil && !errors.Is(err, context.Canceled)
}
