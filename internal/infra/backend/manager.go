package backend

// Manager 是 app/backend.Manager 端口的实现：互斥守护的当前配置指针 +
// PostgreSQL 持久化 + 保存后热更新回调 + 多副本收敛轮询。
//
// 一致性模型与旧栈 PlatformState 相同：
//   - 读路径（List）在锁内做一次带全局 revision 栅栏的一致性快照读取，
//     并把结果 apply 回运行配置（数据库是 Provider 字段的权威源）；
//   - 写路径（Update）CAS 提交成功后重新读取栅栏快照再发布，避免覆盖
//     其他副本在本次写入期间提交的不同 Provider 变更；重读失败保留旧
//     快照，由 RefreshLoop 兜底重试；
//   - RefreshLoop 用轻量全局 revision 轮询（2s）让同库多副本最终收敛，
//     只有 revision 变化才解密并校验完整快照。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	appbackend "ir-agent-server/internal/app/backend"
	"ir-agent-server/internal/domain/audit"
)

// Manager 管理执行后端的运行配置、持久化与热更新。
type Manager struct {
	mu       sync.Mutex
	store    *Store
	current  *PlatformConfig
	applied  int64 // 已应用快照的全局 revision（RefreshLoop 的收敛基准）
	onApply  func(*PlatformConfig)
	logger   *slog.Logger
	interval time.Duration
}

// NewManager 组装管理器。initial 必须已经是加载过数据库 Provider 字段的
// 启动快照（见 LoadInitial）；appliedRevision 是该快照的全局 revision。
// onApply 在每次发布新配置时回调（执行引擎热更新入口），可为 nil。
func NewManager(store *Store, initial *PlatformConfig, appliedRevision int64, onApply func(*PlatformConfig)) *Manager {
	return &Manager{
		store: store, current: initial, applied: appliedRevision,
		onApply: onApply, logger: slog.Default(), interval: 2 * time.Second,
	}
}

// LoadInitial 在启动时读取带全局 revision 栅栏的一致 Provider 快照。
// base 是文件/环境变量加载的原始配置。
func LoadInitial(ctx context.Context, store *Store, base *PlatformConfig) (*PlatformConfig, int64, error) {
	next, _, revision, err := LoadConsistentProviderSnapshot(ctx, store, base)
	if err != nil {
		return nil, 0, err
	}
	return next, revision, nil
}

// Snapshot 返回当前生效配置指针（不可变约定：调用方不得修改）。
func (m *Manager) Snapshot() *PlatformConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// List 实现 app/backend.Manager：锁内一致性快照读取 + apply + 视图构建
// （视图带每项 revision）。密钥字段永不出现在视图里（publicProviderConfig
// 只暴露 *_configured 布尔）。
func (m *Manager) List(ctx context.Context) ([]appbackend.View, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, revisions, snapshotRevision, err := LoadConsistentProviderSnapshot(ctx, m.store, m.current)
	if err != nil {
		return nil, fmt.Errorf("%w: load snapshot: %v", appbackend.ErrStorage, err)
	}
	m.applyLocked(current, snapshotRevision)
	views := ListProviders(current)
	for index := range views {
		views[index].Revision = revisions[views[index].ID]
	}
	return views, nil
}

// RedactForViewer 把视图降级给非管理员：可观察可用性与岗位分配，但不暴露
// 基础设施 URL、本机二进制路径与可编辑配置。
func RedactForViewer(views []appbackend.View) []appbackend.View {
	for index := range views {
		views[index].Config = map[string]any{}
		views[index].Editable = false
	}
	return views
}

// Update 实现 app/backend.Manager：校验 → 组展开 → CAS 保存（同事务审计）
// → 栅栏重读发布。ifMatchRevision 为 nil 时按快照 revision 写（无 If-Match
// 的调用方），遇到并发冲突最多重试两次；显式 If-Match 冲突直接返回
// app/backend.ErrStaleRevision（API 层映射 412）。
func (m *Manager) Update(ctx context.Context, id string, input map[string]any,
	ifMatchRevision *int64, actor string,
) (int64, *appbackend.View, error) {
	if !providerKnown(id) {
		return 0, nil, appbackend.ErrUnknownProvider
	}
	detail, err := json.Marshal(map[string]any{"providerId": id})
	if err != nil {
		return 0, nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	var next *PlatformConfig
	var revision int64
	for attempt := 0; ; attempt++ {
		current, revisions, _, loadErr := LoadConsistentProviderSnapshot(ctx, m.store, m.current)
		if loadErr != nil {
			return 0, nil, fmt.Errorf("%w: load snapshot: %v", appbackend.ErrStorage, loadErr)
		}
		next, _, err = UpdateProvider(current, id, input)
		if err != nil {
			return 0, nil, err
		}
		stored, groupErr := StoredProviderUpdateGroup(next, id)
		if groupErr != nil {
			return 0, nil, groupErr
		}
		writeRevision := ifMatchRevision
		if writeRevision == nil {
			snapshotRevision := revisions[id]
			writeRevision = &snapshotRevision
		}
		revision, err = m.store.PutExecutorProviderGroupIfRevisionAudited(
			ctx, id, stored, writeRevision, audit.Event{
				Actor: actor, Action: audit.Action("provider.update"), Target: id,
				Detail: detail,
			})
		if err == nil {
			break
		}
		if ifMatchRevision == nil && errors.Is(err, ErrStaleProviderRevision) && attempt < 2 {
			continue
		}
		if errors.Is(err, ErrStaleProviderRevision) {
			return 0, nil, appbackend.ErrStaleRevision
		}
		return 0, nil, fmt.Errorf("%w: save group: %v", appbackend.ErrStorage, err)
	}
	// 提交后重新读取带 revision 栅栏的快照再发布；失败保留旧快照（后台
	// RefreshLoop 会重试），不影响本次写入已成功的事实。
	if latest, _, snapshotRevision, loadErr := LoadConsistentProviderSnapshot(ctx, m.store, next); loadErr == nil {
		m.applyLocked(latest, snapshotRevision)
	} else {
		m.logger.Warn("reload Provider state after save failed", "provider", id, "error", loadErr)
	}
	for _, view := range ListProviders(next) {
		if view.ID == id {
			view.Revision = revision
			result := view
			return revision, &result, nil
		}
	}
	return 0, nil, appbackend.ErrUnknownProvider
}

// RefreshLoop 通过轻量全局 revision 轮询让同库多副本最终收敛；ctx 取消
// 时退出。阻塞调用，main 里以 goroutine 启动。
func (m *Manager) RefreshLoop(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		revision, err := m.store.ExecutorProviderRevision(pollCtx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				m.logger.Error("read Provider revision failed", "error", err)
			}
			continue
		}
		m.mu.Lock()
		if revision == m.applied {
			m.mu.Unlock()
			continue
		}
		pollCtx, cancel = context.WithTimeout(ctx, 5*time.Second)
		next, _, snapshotRevision, loadErr := LoadConsistentProviderSnapshot(pollCtx, m.store, m.current)
		cancel()
		if loadErr == nil {
			m.applyLocked(next, snapshotRevision)
			m.logger.Info("Provider configuration refreshed from PostgreSQL", "revision", snapshotRevision)
		} else if ctx.Err() == nil {
			m.logger.Error("refresh Provider configuration failed", "error", loadErr)
		}
		m.mu.Unlock()
	}
}

// applyLocked 发布新配置快照并触发热更新回调。调用方必须持有 m.mu。
func (m *Manager) applyLocked(next *PlatformConfig, revision int64) {
	m.current = next
	m.applied = revision
	if m.onApply != nil {
		m.onApply(next)
	}
}

// 静态断言：Manager 满足应用层端口。
var _ appbackend.Manager = (*Manager)(nil)
