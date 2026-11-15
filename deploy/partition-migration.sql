-- agent_runs 按月分区迁移（DBA 在维护窗口执行）
--
-- 前提：agent_runs 当前是普通表。PG 不支持在线转换分区表，
-- 需要在维护窗口做：创建新分区表 → 迁移数据 → 切换表名。
--
-- 执行步骤：
--   1. 停止 Core（或切换到只读）
--   2. 执行本脚本
--   3. 重启 Core
--
-- 预计耗时：百万行约 30 秒（PG SSD）。

BEGIN;

-- 1. 创建分区父表
CREATE TABLE agent_runs_partitioned (
    LIKE agent_runs INCLUDING ALL
) PARTITION BY RANGE (created_at);

-- 注意：agent_runs.created_at 是 TEXT (RFC3339)。
-- 分区键需要时间类型——用表达式分区或改为 TIMESTAMPTZ。
-- 如果不想改列类型，可以用触发器子分区（见下方注释）。

-- 2. 创建初始分区（当前月 + 前 3 个月 + 后 3 个月）
-- 需要根据实际 created_at 格式调整。
-- CREATE TABLE agent_runs_2026_11 PARTITION OF agent_runs_partitioned
--     FOR VALUES FROM ('2026-11-01') TO ('2026-12-01');
-- CREATE TABLE agent_runs_2026_12 PARTITION OF agent_runs_partitioned
--     FOR VALUES FROM ('2026-12-01') TO ('2027-01-01');
-- ... (按需添加)

-- 3. 迁移数据
-- INSERT INTO agent_runs_partitioned SELECT * FROM agent_runs;

-- 4. 切换表名
-- ALTER TABLE agent_runs RENAME TO agent_runs_old;
-- ALTER TABLE agent_runs_partitioned RENAME TO agent_runs;

-- 5. 验证后删除旧表
-- DROP TABLE agent_runs_old;

COMMIT;

-- 替代方案（不改表结构）：
-- 如果不想做分区，最简单的保护是启用 retention（PLATFORM__RETENTION_DAYS=90）
-- + 添加 created_at 索引（已有 idx_agent_runs_created）+ 定期 VACUUM/ANALYZE。
-- 对于 10K 用户（~50K rows/天 × 90 天 = 4.5M 行），PG 的索引和
-- autovacuum 已经足够——分区在数据量 > 1000 万行时才必要。

-- 定期分区管理（放入 crontab 或 pg_cron）：
-- SELECT create_monthly_partition('agent_runs', to_char(now() + interval '1 month', 'YYYY_MM'));
