package storage

import (
	"context"
	"encoding/json"
	"strings"
)

// NodeAlertState 节点故障告警的持久化状态：判定为故障时写入，恢复后删除。
// 存在 KV 里，进程重启后既不会重复发「节点故障」，恢复时也还能发出「节点恢复」。
type NodeAlertState struct {
	// Since 本次故障里第一个失败样本的时间。
	Since int64 `json:"since"`
	// DownAt 判定为故障的时间。
	DownAt int64 `json:"downAt"`
}

func nodeAlertPrefix(uid string) string {
	return "alert:down:" + uid + ":"
}

func nodeAlertKey(uid, name string) string {
	return nodeAlertPrefix(uid) + strings.ToLower(strings.TrimSpace(name))
}

// ListNodeAlertStates 返回 uid 下所有处于故障中的节点，键为节点名。
// 内容损坏的记录直接跳过，相当于这个节点没有在告警。
func (s *Store) ListNodeAlertStates(ctx context.Context, uid string) (map[string]NodeAlertState, error) {
	prefix := nodeAlertPrefix(uid)
	out := map[string]NodeAlertState{}
	cursor := 0
	for {
		res, err := s.KV().List(ctx, prefix, cursor, 1000)
		if err != nil {
			return nil, err
		}
		for _, key := range res.Keys {
			value, ok, err := s.KV().Get(ctx, key)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			var st NodeAlertState
			if json.Unmarshal([]byte(value), &st) != nil {
				continue
			}
			out[strings.TrimPrefix(key, prefix)] = st
		}
		if res.ListComplete {
			return out, nil
		}
		cursor = res.Cursor
	}
}

func (s *Store) SaveNodeAlertState(ctx context.Context, uid, name string, st NodeAlertState) error {
	return s.KV().Put(ctx, nodeAlertKey(uid, name), st)
}

func (s *Store) DeleteNodeAlertState(ctx context.Context, uid, name string) error {
	return s.KV().Delete(ctx, nodeAlertKey(uid, name))
}

// GetNodeAlertState 读取单个节点的故障状态，ok 为 false 表示这个节点没有在告警。
func (s *Store) GetNodeAlertState(ctx context.Context, uid, name string) (NodeAlertState, bool, error) {
	var st NodeAlertState
	ok, err := s.KV().GetJSON(ctx, nodeAlertKey(uid, name), &st)
	return st, ok, err
}
