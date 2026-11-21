# IR (Intelligent Router) — API 文档

## Base URL

```
生产: https://ir.example.com
本地: http://127.0.0.1:8080
```

## 认证

### 管理台 Session（Cookie + CSRF）

```
POST /api/admin/v2/login
{"username":"admin@example.com","password":"..."}
→ Set-Cookie: session=...; data.csrfToken
```

后续写操作需带 `X-CSRF-Token` 头。

### 智能体密钥（Edge 连接器）

```
Authorization: Bearer <agent-key>
X-Agent-Slug: <slug>
X-Agent-Cluster: <cluster>     # 可选（集群子密钥）
```

### API Key（程序化调用）

```
Authorization: Bearer irak_<key>
```

---

## 核心 API

### 智能体管理

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | /api/admin/v2/agents | 列表（分页+搜索） |
| POST | /api/admin/v2/agents | 创建 |
| GET | /api/admin/v2/agents/{slug} | 详情 |
| PUT | /api/admin/v2/agents/{slug} | 更新 |
| DELETE | /api/admin/v2/agents/{slug} | 删除 |
| GET | /api/admin/v2/agents/{slug}/secret | 查看签名密钥 |
| GET | /api/admin/v2/agents/export?format=json|csv | 导出 |
| POST | /api/admin/v2/agents/import | 批量导入 |

### 团队管理

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | /api/admin/v2/teams | 列表 |
| POST | /api/admin/v2/teams | 创建（含 DAG 步骤） |
| GET | /api/admin/v2/teams/{id} | 详情 |
| PUT | /api/admin/v2/teams/{id} | 更新 |
| DELETE | /api/admin/v2/teams/{id} | 删除 |
| POST | /api/admin/v2/teams/{id}/share | 创建分享链接 |

### 团队运行

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | /api/admin/v2/team-runs | 运行列表 |
| POST | /api/admin/v2/team-runs | 提交运行（需幂等键） |
| GET | /api/admin/v2/team-runs/{id} | 运行详情（含步骤状态） |
| GET | /api/admin/v2/team-runs/{id}/events | **SSE 实时事件流** |
| POST | /api/admin/v2/team-runs/{id}/cancel | 取消 |
| POST | /api/admin/v2/team-runs/{id}/retry | 重试 |

**提交运行示例：**

```json
POST /api/admin/v2/team-runs
X-Idempotency-Key: unique-key-123

{
  "team_id": "stock-analysis",
  "input": {"symbol": "AAPL", "market": "US"}
}
```

**SSE 订阅示例：**

```javascript
const es = new EventSource('/api/admin/v2/team-runs/{id}/events');
es.addEventListener('succeeded', e => console.log('步骤完成:', JSON.parse(e.data)));
es.addEventListener('completed', e => { es.close(); console.log('运行完成'); });
```

### Agent API（Edge 连接器）

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | /agent-api/v1/claim | 长轮询认领任务 |
| POST | /agent-api/v1/renew | 续租 |
| POST | /agent-api/v1/complete | 幂等回传结果 |
| POST | /agent-api/v1/heartbeat | 存活与容量上报 |
| POST | /agent-api/v1/runs | 机器侧直接提交（cron） |

### 动态入口（签名调用）

```
POST /agent/{slug}
X-Agent-Signature: <hex>            # 裸 hex 签名，不带 "v1 " 前缀
X-Agent-Timestamp: <unix-seconds>
X-Agent-Nonce: <16-128 URL-safe chars>
```

Canonical 串格式（HMAC-SHA256，密钥为智能体签名密钥）：
```
v1\nPOST\n/agent/{slug}\n{timestamp}\n{nonce}\n{runId}\n{idempotencyKey}\n{bodySha256Hex}
```

### 公开分享

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | /api/share/v1/{token} | 团队公开视图（脱敏） |
| POST | /api/share/v1/{token}/runs | 触发运行（限流） |
| GET | /api/share/v1/{token}/runs/{id} | 轮询结果 |

### 用户认证

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | /api/admin/v2/auth/register | 用户注册（role=user） |
| POST | /api/admin/v2/auth/api-keys | 生成 API Key |

### 运行日志

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | /api/admin/v2/runs | 智能体运行日志 |
| GET | /api/admin/v2/audits | 审计日志 |
| GET | /api/admin/v2/channel-events | 通道事件 |

---

## 限流

| 维度 | 默认 | 环境变量 |
|---|---|---|
| IP | 60 请求/分钟 | PLATFORM__RATE_LIMIT_IP_PER_MINUTE |
| 用户 | 30 请求/分钟 | PLATFORM__RATE_LIMIT_USER_PER_MINUTE |

超限返回 `429` + `Retry-After: 60`。

## 健康检查

```
GET /health/ready
→ {"checks":[...],"status":"healthy"}
```

## 监控

```
GET /metrics    # Prometheus 格式（需内网访问）
```
