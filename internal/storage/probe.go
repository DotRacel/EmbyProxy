package storage

import (
	"context"
	"strings"
	"time"
)

// ProbeSample 一条落库的上游探测样本。Target 为空表示节点整体（故障转移后实际生效的那条线路）的样本。
type ProbeSample struct {
	Node   string
	Target string
	At     int64
	MS     int64
	Status int
	OK     bool
	Err    string
}

// AppendProbeSamples 把一轮探测的样本批量写入，同一 (node, target, at) 重复写入时覆盖。
func (s *Store) AppendProbeSamples(ctx context.Context, samples []ProbeSample) error {
	if s == nil || s.db == nil || len(samples) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, sample := range samples {
		if sample.Node == "" || sample.At <= 0 {
			continue
		}
		ok := 0
		if sample.OK {
			ok = 1
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO probe_samples (node, target, at, ms, status, ok, err)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(node, target, at) DO UPDATE SET
				ms = excluded.ms,
				status = excluded.status,
				ok = excluded.ok,
				err = excluded.err
		`, sample.Node, sample.Target, sample.At, sample.MS, sample.Status, ok, sample.Err); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LoadProbeSamples 读取 maxAge 时间内的全部样本，按时间升序返回，供进程启动时回填内存曲线。
func (s *Store) LoadProbeSamples(ctx context.Context, maxAge time.Duration) ([]ProbeSample, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	if maxAge <= 0 {
		maxAge = 24 * time.Hour
	}
	cutoff := time.Now().Add(-maxAge).UnixMilli()
	rows, err := s.db.QueryContext(ctx, `
		SELECT node, target, at, ms, status, ok, COALESCE(err, '')
		FROM probe_samples WHERE at >= ? ORDER BY at ASC
	`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ProbeSample{}
	for rows.Next() {
		var sample ProbeSample
		var ok int
		if err := rows.Scan(&sample.Node, &sample.Target, &sample.At, &sample.MS, &sample.Status, &ok, &sample.Err); err != nil {
			return nil, err
		}
		sample.OK = ok != 0
		out = append(out, sample)
	}
	return out, rows.Err()
}

// PruneProbeSamples 删除超出保留窗口的样本。
func (s *Store) PruneProbeSamples(ctx context.Context, maxAge time.Duration) error {
	if s == nil || s.db == nil {
		return nil
	}
	if maxAge <= 0 {
		maxAge = 24 * time.Hour
	}
	cutoff := time.Now().Add(-maxAge).UnixMilli()
	_, err := s.db.ExecContext(ctx, `DELETE FROM probe_samples WHERE at < ?`, cutoff)
	return err
}

// RetainProbeSamples 删除不在 names 里的节点的样本，对应节点被删除或改名。
// names 为空时不做任何事——那更可能是一次读取失败，而不是真的一个节点都没有。
func (s *Store) RetainProbeSamples(ctx context.Context, names []string) error {
	if s == nil || s.db == nil || len(names) == 0 {
		return nil
	}
	args := make([]any, 0, len(names))
	for _, name := range names {
		args = append(args, name)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")
	_, err := s.db.ExecContext(ctx, `DELETE FROM probe_samples WHERE node NOT IN (`+placeholders+`)`, args...)
	return err
}

// DeleteProbeSamples 删除单个节点的全部样本。
func (s *Store) DeleteProbeSamples(ctx context.Context, name string) error {
	if s == nil || s.db == nil || name == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM probe_samples WHERE node = ?`, name)
	return err
}
