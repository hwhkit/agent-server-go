package mota

// Agent Pull Connector API 的配置加载与校验。
// 端点契约见 doc/AGENT-PULL-API.md。

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// AgentAPIConfig 是 /agent-api/v1 连接器端点的运行配置。
type AgentAPIConfig struct {
	Enabled                bool
	LeaseSeconds           int
	MaxAttempts            int
	ClaimMaxWaitSeconds    int
	QueueTTLSeconds        int
	SyncHoldMaxSeconds     int
	MaxOutstandingLeases   int
	WorkerOfflineAfterMiss int
	TokenRotationGraceSecs int
}

const (
	defaultLeaseSeconds           = 90
	defaultMaxAttempts            = 3
	defaultClaimMaxWaitSeconds    = 30
	defaultQueueTTLSeconds        = 3600
	defaultSyncHoldMaxSeconds     = 300
	defaultMaxOutstandingLeases   = 64
	defaultWorkerOfflineMisses    = 3
	defaultTokenRotationGraceSecs = 86400
)

// LoadAgentAPIConfig 从 AGENT_API__* 环境变量加载配置;全部提供默认值。
func LoadAgentAPIConfig() (*AgentAPIConfig, error) {
	cfg := &AgentAPIConfig{
		Enabled:                strings.TrimSpace(os.Getenv("AGENT_API__ENABLED")) == "true",
		LeaseSeconds:           defaultLeaseSeconds,
		MaxAttempts:            defaultMaxAttempts,
		ClaimMaxWaitSeconds:    defaultClaimMaxWaitSeconds,
		QueueTTLSeconds:        defaultQueueTTLSeconds,
		SyncHoldMaxSeconds:     defaultSyncHoldMaxSeconds,
		MaxOutstandingLeases:   defaultMaxOutstandingLeases,
		WorkerOfflineAfterMiss: defaultWorkerOfflineMisses,
		TokenRotationGraceSecs: defaultTokenRotationGraceSecs,
	}
	ints := []struct {
		name string
		dst  *int
	}{
		{"AGENT_API__LEASE_SECONDS", &cfg.LeaseSeconds},
		{"AGENT_API__MAX_ATTEMPTS", &cfg.MaxAttempts},
		{"AGENT_API__CLAIM_MAX_WAIT_SECONDS", &cfg.ClaimMaxWaitSeconds},
		{"AGENT_API__QUEUE_TTL_SECONDS", &cfg.QueueTTLSeconds},
		{"AGENT_API__SYNC_HOLD_MAX_SECONDS", &cfg.SyncHoldMaxSeconds},
		{"AGENT_API__MAX_OUTSTANDING_LEASES", &cfg.MaxOutstandingLeases},
		{"AGENT_API__WORKER_OFFLINE_AFTER_MISSES", &cfg.WorkerOfflineAfterMiss},
		{"AGENT_API__TOKEN_ROTATION_GRACE", &cfg.TokenRotationGraceSecs},
	}
	for _, item := range ints {
		raw := strings.TrimSpace(os.Getenv(item.name))
		if raw == "" {
			continue
		}
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			return nil, fmt.Errorf("%s must be a positive integer, got %q", item.name, raw)
		}
		*item.dst = value
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate 校验语义约束。
func (c *AgentAPIConfig) Validate() error {
	if c.LeaseSeconds < 3 {
		return errors.New("AGENT_API__LEASE_SECONDS must be >= 3 (>= 3x renew interval)")
	}
	if c.MaxAttempts < 1 || c.MaxAttempts > 10 {
		return errors.New("AGENT_API__MAX_ATTEMPTS must be between 1 and 10")
	}
	if c.ClaimMaxWaitSeconds < 1 || c.ClaimMaxWaitSeconds > 35 {
		return errors.New("AGENT_API__CLAIM_MAX_WAIT_SECONDS must be between 1 and 35")
	}
	if c.SyncHoldMaxSeconds < 1 {
		return errors.New("AGENT_API__SYNC_HOLD_MAX_SECONDS must be >= 1")
	}
	if c.MaxOutstandingLeases < 1 {
		return errors.New("AGENT_API__MAX_OUTSTANDING_LEASES must be >= 1")
	}
	if c.WorkerOfflineAfterMiss < 1 {
		return errors.New("AGENT_API__WORKER_OFFLINE_AFTER_MISSES must be >= 1")
	}
	return nil
}

// LeaseTTL / RenewInterval / QueueTTL / SyncHoldMax 以时间量返回。
func (c *AgentAPIConfig) LeaseTTL() time.Duration {
	return time.Duration(c.LeaseSeconds) * time.Second
}

// RenewInterval 是建议续租间隔 = 租约的 1/3,至少 1s。
func (c *AgentAPIConfig) RenewInterval() time.Duration {
	interval := c.LeaseSeconds / 3
	if interval < 1 {
		interval = 1
	}
	return time.Duration(interval) * time.Second
}

func (c *AgentAPIConfig) QueueTTL() time.Duration {
	return time.Duration(c.QueueTTLSeconds) * time.Second
}

func (c *AgentAPIConfig) SyncHoldMax() time.Duration {
	return time.Duration(c.SyncHoldMaxSeconds) * time.Second
}

func (c *AgentAPIConfig) ClaimMaxWait() time.Duration {
	return time.Duration(c.ClaimMaxWaitSeconds) * time.Second
}
