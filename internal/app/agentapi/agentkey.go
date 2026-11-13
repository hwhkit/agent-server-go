package agentapi

// 智能体密钥连接点（方案 B）：执行器以智能体签名密钥认证 agent-api。
// 主密钥（X-Agent-Cluster 缺省）或集群派生子密钥
// HMAC-SHA256(主密钥, "ir-cluster-v1\0" + clusterID) 二选一；
// 派生子密钥可按集群吊销（ClusterDenyRepository 名单）。
// 派生公式与常量时间比较在 domain 层（connector.DeriveClusterKey 等）。

import (
	"context"
	"strings"

	domain "ir-agent-server/internal/domain/connector"
)

// AgentSecretSource 提供智能体签名密钥（解密后明文，仅执行路径可见）。
// prev 是轮换宽限窗口内的上一代密钥（无则为 nil）。
type AgentSecretSource interface {
	SigningSecrets(ctx context.Context, slug string) (current, prev []byte, err error)
}

// AgentKeyHint 是 agent-key 认证的请求线索（HTTP 层从 X-Agent-Slug /
// X-Agent-Cluster 头提取）。act_ 令牌路径忽略它。
type AgentKeyHint struct {
	Slug    string
	Cluster string
}

// AuthenticateAgentKey 以智能体主密钥或集群派生子密钥鉴权。
// 主体验 ID 为 "agent:<slug>"（workers 心跳表与令牌 ID 共列、TEXT 存储），
// scope 锁定为该 slug。密钥轮换宽限期内旧主密钥/其派生子密钥仍可用。
func (s *Service) AuthenticateAgentKey(ctx context.Context, key, slug, cluster string) (*Principal, *Failure) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		return nil, failure(401, "unauthorized", "agent key auth requires X-Agent-Slug header")
	}
	if s.secrets == nil {
		return nil, failure(401, "unauthorized", "agent key auth is not available")
	}
	current, prev, err := s.secrets.SigningSecrets(ctx, slug)
	if err != nil || len(current) == 0 {
		return nil, failure(401, "unauthorized", "invalid agent key")
	}
	principal := Principal{ID: "agent:" + slug, Name: slug, Scopes: []string{slug}}
	if cluster == "" {
		if domain.VerifyMasterKey(current, key) ||
			(len(prev) > 0 && domain.VerifyMasterKey(prev, key)) {
			return &principal, nil
		}
		return nil, failure(401, "unauthorized", "invalid agent key")
	}
	// 集群子密钥路径。持主密钥者视为可信（等同在任意集群执行），不受名单
	// 影响——名单的语义是只吊销该集群的派生子密钥。
	if err := domain.ValidateClusterID(cluster); err != nil {
		return nil, failure(400, "invalid_cluster", err.Error())
	}
	masterOk := domain.VerifyMasterKey(current, key) ||
		(len(prev) > 0 && domain.VerifyMasterKey(prev, key))
	if !masterOk {
		if s.denies != nil {
			denied, derr := s.denies.Denied(ctx, slug, cluster)
			if derr == nil && denied {
				return nil, failure(403, "cluster_denied",
					"cluster "+cluster+" is denied for agent "+slug)
			}
		}
		if !domain.VerifyClusterKey(current, cluster, key) &&
			!(len(prev) > 0 && domain.VerifyClusterKey(prev, cluster, key)) {
			return nil, failure(401, "unauthorized", "invalid agent key")
		}
	}
	principal.Name = slug + "/" + cluster
	return &principal, nil
}

// DenyCluster 禁用一个集群绑定（方案 B 按集群吊销）。
func (s *Service) DenyCluster(ctx context.Context, slug, clusterID string) error {
	if s.denies == nil {
		return errDeniesUnavailable
	}
	return s.denies.Deny(ctx, slug, clusterID)
}

// AllowCluster 解除集群禁用。
func (s *Service) AllowCluster(ctx context.Context, slug, clusterID string) error {
	if s.denies == nil {
		return errDeniesUnavailable
	}
	return s.denies.Allow(ctx, slug, clusterID)
}

// ListDeniedClusters 返回全部禁用绑定。
func (s *Service) ListDeniedClusters(ctx context.Context) ([]domain.ClusterDeny, error) {
	if s.denies == nil {
		return nil, errDeniesUnavailable
	}
	return s.denies.List(ctx)
}
