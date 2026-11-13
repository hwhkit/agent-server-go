package postgres

// 集群禁用名单存储（agent_pull_cluster_deny 表）：方案 B 的按集群吊销。
// 命中名单的派生子密钥在鉴权时被拒，主密钥不受影响。

import (
	"context"
	"fmt"
	"time"

	"ir-agent-server/internal/domain/connector"
)

// ClusterDenyStore 实现 connector.ClusterDenyRepository。
type ClusterDenyStore struct{ db *DB }

// NewClusterDenyStore 构建集群禁用名单仓储。
func NewClusterDenyStore(db *DB) *ClusterDenyStore { return &ClusterDenyStore{db: db} }

var _ connector.ClusterDenyRepository = (*ClusterDenyStore)(nil)

// Deny 禁用一个集群绑定（幂等）。
func (s *ClusterDenyStore) Deny(ctx context.Context, slug, clusterID string) error {
	_, err := s.db.Pool.Exec(ctx,
		`INSERT INTO agent_pull_cluster_deny (agent_slug, cluster_id, denied_at)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (agent_slug, cluster_id) DO NOTHING`,
		slug, clusterID, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("deny cluster: %w", err)
	}
	return nil
}

// Allow 解除禁用（幂等）。
func (s *ClusterDenyStore) Allow(ctx context.Context, slug, clusterID string) error {
	_, err := s.db.Pool.Exec(ctx,
		`DELETE FROM agent_pull_cluster_deny WHERE agent_slug = $1 AND cluster_id = $2`,
		slug, clusterID)
	if err != nil {
		return fmt.Errorf("allow cluster: %w", err)
	}
	return nil
}

// List 返回全部禁用绑定。
func (s *ClusterDenyStore) List(ctx context.Context) ([]connector.ClusterDeny, error) {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT agent_slug, cluster_id, denied_at
		 FROM agent_pull_cluster_deny ORDER BY agent_slug, cluster_id`)
	if err != nil {
		return nil, fmt.Errorf("list denied clusters: %w", err)
	}
	defer rows.Close()
	out := make([]connector.ClusterDeny, 0, 4)
	for rows.Next() {
		var item connector.ClusterDeny
		var deniedAt string
		if err := rows.Scan(&item.AgentSlug, &item.ClusterID, &deniedAt); err != nil {
			return nil, err
		}
		item.DeniedAt, _ = time.Parse(time.RFC3339, deniedAt)
		out = append(out, item)
	}
	return out, rows.Err()
}

// Denied 报告该集群绑定是否被禁用。
func (s *ClusterDenyStore) Denied(ctx context.Context, slug, clusterID string) (bool, error) {
	var denied bool
	err := s.db.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM agent_pull_cluster_deny
		 WHERE agent_slug = $1 AND cluster_id = $2)`,
		slug, clusterID).Scan(&denied)
	if err != nil {
		return false, fmt.Errorf("check denied cluster: %w", err)
	}
	return denied, nil
}
