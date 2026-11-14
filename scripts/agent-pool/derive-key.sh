#!/usr/bin/env bash
# derive-key.sh —— 从智能体主密钥派生集群子密钥（方案 B）。
# 子密钥 = base64url(HMAC-SHA256(masterKey, "ir-cluster-v1\0" + clusterID))
# 平台按同一公式重算验证；集群吊销 = 平台名单删除该 clusterID；主密钥永不离开本机。
#
# 用法：
#   derive-key.sh <masterKey> <clusterID>
#   AGENT_KEY=$(cat ~/ir-pool/master.key); ./derive-key.sh "$AGENT_KEY" alpha > ~/ir-pool/cluster.key
# 服务端实现：internal/domain/connector/agentkey.go（DeriveClusterKey）。
set -u
master="${1:?need masterKey}"
cluster="${2:?need clusterID ([a-z0-9-])}"

# 服务端校验: ^[a-z0-9][a-z0-9-]{0,63}$
if ! echo "$cluster" | grep -qE '^[a-z0-9][a-z0-9-]{0,63}$'; then
  echo "clusterID must be 1-64 chars of [a-z0-9-]" >&2
  exit 2
fi

# 用 python3（sec03 管线同款依赖）保证与 Go 侧字节级一致；密钥经 argv 传递。
python3 -c '
import base64, hashlib, hmac, sys
master, cluster = sys.argv[1].encode(), sys.argv[2].encode()
mac = hmac.new(master, b"ir-cluster-v1\x00" + cluster, hashlib.sha256).digest()
print(base64.urlsafe_b64encode(mac).rstrip(b"=").decode())
' "$master" "$cluster"
