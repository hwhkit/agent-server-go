package connector

// 智能体密钥连接点（方案 B）：执行器以智能体签名密钥（主密钥）或其
// 集群派生子密钥认证 agent-api。主密钥永离平台；集群持有
// HMAC-SHA256(主密钥, "ir-cluster-v1\0" + clusterID) 的派生值，
// 平台可重算验证、按 clusterID 吊销（禁用名单）。

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"regexp"
	"time"
)

// clusterKeyInfo 是派生域分隔符；换公式即全体子密钥失效。
const ClusterKeyInfo = "ir-cluster-v1\x00"

var clusterIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// ValidateClusterID 校验集群标识（workerId 同风格，允许 1 字符）。
func ValidateClusterID(id string) error {
	if !clusterIDPattern.MatchString(id) {
		return errors.New("cluster id must be 1-64 chars of [a-z0-9-]")
	}
	return nil
}

// DeriveClusterKey 从智能体主密钥派生集群子密钥（base64url，无填充）。
func DeriveClusterKey(secret []byte, clusterID string) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(ClusterKeyInfo + clusterID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// VerifyClusterKey 常数时间校验集群子密钥。
func VerifyClusterKey(secret []byte, clusterID, presented string) bool {
	expected := DeriveClusterKey(secret, clusterID)
	return subtle.ConstantTimeCompare([]byte(expected), []byte(presented)) == 1
}

// VerifyMasterKey 常数时间校验主密钥（长度不同即 false）。
func VerifyMasterKey(secret []byte, presented string) bool {
	return subtle.ConstantTimeCompare([]byte(presented), secret) == 1
}

// ClusterDeny 是被禁用的集群绑定（slug + clusterID）。
type ClusterDeny struct {
	AgentSlug string
	ClusterID string
	DeniedAt  time.Time
}

// ClusterDenyRepository 管理集群禁用名单；Denied 命中即拒绝该子密钥。
type ClusterDenyRepository interface {
	Deny(ctx context.Context, slug, clusterID string) error
	Allow(ctx context.Context, slug, clusterID string) error
	List(ctx context.Context) ([]ClusterDeny, error)
	Denied(ctx context.Context, slug, clusterID string) (bool, error)
}
