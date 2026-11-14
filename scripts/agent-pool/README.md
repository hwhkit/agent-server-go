# IR Agent Pool —— DeAlan 调度脚本的 Intelligent Router 适配版

把三台机器上的 DeAlan 执行池（pi-pool / cc-pool）从 dispatcher 队列切到
IR 平台的 Pull Connector API（`doc/AGENT-PULL-API.md`），连接点是**智能体密钥**
（方案 B：主密钥或集群派生子密钥）。原 `run-task.sh` 的 sec03 管线
（runner 预计算 → trim_prompt → LLM 链 → parse4 合并）**零改动复用**。

## 文件

| 文件 | 作用 |
|---|---|
| `pool.sh` | 调度环：长轮询 claim → 补丁版 run-task.sh 执行 → renew/abort → 转 IR complete + heartbeat 循环 |
| `submit.sh` | cron/本地事件直接提交运行（`POST /agent-api/v1/runs`），crontab 一行包装定时任务 |
| `derive-key.sh` | 从智能体主密钥派生集群子密钥（与平台 Go 侧字节级一致） |
| `env.example` | 脱敏环境模板（真实凭据永不入库） |

## 部署

```bash
# 1. 平台侧：管理台创建智能体（表单固定 remote_pull），记下签名密钥（主密钥）
# 2. 各集群机器（每集群一份目录）:
mkdir -p ~/ir-pool && cd ~/ir-pool
cp <repo>/scripts/agent-pool/{pool.sh,submit.sh,derive-key.sh} .
cat > pool-env <<EOF   # 真实凭据写这里（chmod 600）
IR_SERVER=https://ir.example.com
AGENT_SLUG=my-agent
AGENT_KEY=<主密钥或派生子密钥>
CLUSTER=pi01
POOL_DIR=$HOME/ir-pool
RUN_TASK_ORIG=$HOME/agent-dispatcher-workers/cc-pool/run-task.sh
MAX_SLOTS=10
EOF
set -a; . ./pool-env; set +a
./pool.sh &            # 或 systemd 单元（Restart=always）
# 3. 派生子密钥（推荐：按集群吊销）
./derive-key.sh "$AGENT_KEY" pi01 > cluster.key   # AGENT_KEY 改填 cluster.key
# 4. 定时任务包装（可选）：
30 1 * * * /path/run.sh | $HOME/ir-pool/submit.sh -slug my-agent -payload - >> $HOME/ir-pool/cron.log 2>&1
```

## DeAlan 语义 → IR 映射（原六条重写对照的落点）

| DeAlan 语义（原实现） | IR 版由谁承担 |
|---|---|
| 1. 槽位锁 `slots/N.lock` + pid 判活 | 服务端租约（claim SKIP LOCKED 独占）+ 本地进程计数；锁文件取消 |
| 2. 孤儿检测（扫 runs/ 无锁目录） | 服务端 reaper：租约过期自动重派/判死，本地零代码 |
| 3. 停用语义（200 空队列无感知） | 管理台停用智能体 → claim 204（`X-Agent-Api-Reason: no-scope-match`）；执行中 renew 回 `abort:true` → 本脚本杀进程组 |
| 4. agent_tag 定向（漏配静默饿死） | `X-Agent-Slug` + `preferredSlugs` 显式定向；不匹配时 204 带诊断头，无饿死陷阱 |
| 5. 三败即死 + 300s 主管杀 | 服务端 `attempts`（MAX_ATTEMPTS=3）+ `executionDeadline` 墙钟（renew 409 TASK_DEADLINE_EXCEEDED）；本地仅杀进程组兜底 |
| 6. sec03 管线 | **原样保留**：原 run-task.sh 打补丁复用（仅"上报 dispatcher"一行替换为落盘 `ir-result.json`），启动时按源 mtime 自动重打 |

## 与 dispatcher 版的切换

- **切 IR**：池目录换成 `ir-pool`（本套件）+ 平台侧创建智能体；原 dispatcher 池可并行保留直至流量迁移完
- **回切**：`RUN_TASK_ORIG` 指向的原 run-task.sh 从未被修改（补丁副本在 ir-pool 目录内），systemd 切回原 pool.sh 即可，sec03 管线无任何分叉
- 补丁特征码 `IR-PATCHED`：源脚本上报行变更时启动拒绝（不会静默打错补丁）

## 观测

- 服务端：管理台「日志 → 运行日志」（每次派发 workerId/结局）、连接器页签（worker 在线/容量）、仪表盘队列数
- 本地：`pool.log`（claim/dispatch/complete 全链路时间线）、`undelivered/`（回报失败留档，人工补投）

## 凭据模型（方案 B）

| 凭据 | 说明 |
|---|---|
| 主密钥 | 智能体签名密钥；入站动态入口签名与连接器认证共用；轮换后旧派生子密钥在宽限期内仍有效 |
| 集群子密钥 | `derive-key.sh` 从主密钥派生（HMAC-SHA256），平台按 clusterID 验算；按集群吊销 = 平台名单删除 |
| act_ 令牌 | 兼容路径（管理台连接器页签签发，可服务多智能体）；agent-key 模式下非必需 |
