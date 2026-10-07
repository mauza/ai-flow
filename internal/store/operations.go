package store

import "context"

// Operations is an exact, unpaginated summary across all flow versions.
type Operations struct {
	AsOf              int64             `json:"as_of"`
	Active            int               `json:"active"`
	Waiting           int               `json:"waiting"`
	Queued            int               `json:"queued"`
	OldestQueuedAgeMS int64             `json:"oldest_queued_age_ms"`
	Nodes             []*NodeOperations `json:"nodes"`
}

type NodeOperations struct {
	Project         string         `json:"project"`
	FlowName        string         `json:"flow_name"`
	Node            string         `json:"node"`
	Type            string         `json:"type"`
	Visits          int            `json:"visits"`
	DurationSamples int            `json:"duration_samples"`
	DurationTotalMS int64          `json:"duration_total_ms"`
	DurationMaxMS   int64          `json:"duration_max_ms"`
	Failures        map[string]int `json:"failures"`
}

// Operations uses two SQL aggregates in one read transaction, without loading
// run snapshots, visit payloads, or per-run queries. The caller owns the deadline.
// Active means running, excluding waiting. Durations include only visits with
// a positive start and finish >= start; failure categories reflect stored facts.
func (s *Store) Operations(ctx context.Context) (*Operations, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := &Operations{AsOf: now(), Nodes: []*NodeOperations{}}
	err = tx.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(status = 'running'), 0),
		COALESCE(SUM(status = 'waiting'), 0),
		COALESCE(SUM(status = 'queued'), 0),
		MAX(0, COALESCE(? - MIN(CASE WHEN status = 'queued' THEN created_at END), 0))
		FROM runs`, out.AsOf).Scan(&out.Active, &out.Waiting, &out.Queued, &out.OldestQueuedAgeMS)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT r.project, r.flow_name, v.node, v.type,
		COUNT(*),
		SUM(v.started_at > 0 AND v.finished_at >= v.started_at),
		SUM(CASE WHEN v.started_at > 0 AND v.finished_at >= v.started_at THEN v.finished_at - v.started_at ELSE 0 END),
		MAX(CASE WHEN v.started_at > 0 AND v.finished_at >= v.started_at THEN v.finished_at - v.started_at ELSE 0 END),
		SUM(v.status = 'canceled'),
		SUM(v.status = 'error' AND v.error != 'node exceeded its timeout'),
		SUM((v.status = 'error' AND v.error = 'node exceeded its timeout') OR (v.status = 'succeeded' AND v.outcome = 'timeout')),
		SUM(v.status = 'succeeded' AND v.outcome = 'fail')
		FROM visits v JOIN runs r ON r.id = v.run_id
		GROUP BY r.project, r.flow_name, v.node, v.type
		ORDER BY r.project, r.flow_name, v.node, v.type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		n := &NodeOperations{Failures: map[string]int{}}
		var canceled, failed, timeout, failOutcome int
		if err := rows.Scan(&n.Project, &n.FlowName, &n.Node, &n.Type, &n.Visits, &n.DurationSamples, &n.DurationTotalMS, &n.DurationMaxMS, &canceled, &failed, &timeout, &failOutcome); err != nil {
			return nil, err
		}
		for category, count := range map[string]int{"canceled": canceled, "error": failed, "timeout": timeout, "fail_outcome": failOutcome} {
			if count > 0 {
				n.Failures[category] = count
			}
		}
		out.Nodes = append(out.Nodes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}
