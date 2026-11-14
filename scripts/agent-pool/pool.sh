#!/usr/bin/env bash
# IR Agent Pool 调度环 —— DeAlan pool.sh 的 Intelligent Router 适配版。
#
# 与 dispatcher 版的差异（语义映射见 README.md）：
#   claim  POST {IR_SERVER}/agent-api/v1/claim（长轮询；Bearer 智能体密钥）
#   槽位   本地并发计数 + 服务端租约（SKIP LOCKED 独占）——不再需要 slots/N.lock
#   重派   服务端 reaper 全托管（attempts/墙钟/孤儿）——本地无超时监视/孤儿扫描
#   停用   renew 回执 abort:true → 杀进程组放弃（管理台停用即生效）
#   执行   原管线复用：启动时对原 run-task.sh 打补丁（上报行改为落盘 ir-result.json），
#          本脚本读补丁版结果转 IR complete（幂等，重试 5 次 2/4/6/8s + undelivered 留档）
#
# 依赖：bash jq curl；机器上原 sec03 管线照常（RUN_TASK_ORIG 指向原版）。
set -u

IR_SERVER="${IR_SERVER:?need IR_SERVER (e.g. https://ir.example.com)}"
AGENT_SLUG="${AGENT_SLUG:?need AGENT_SLUG}"
AGENT_KEY="${AGENT_KEY:?need agent signing key or cluster subkey}"
CLUSTER="${CLUSTER:-}"
MAX_SLOTS="${MAX_SLOTS:-5}"
POOL="${POOL_DIR:?need POOL_DIR (ir-pool working dir)}"
RUN_TASK_ORIG="${RUN_TASK_ORIG:?need RUN_TASK_ORIG (原 dispatcher 版 run-task.sh 路径)}"
TASK_TIMEOUT_SECS="${TASK_TIMEOUT_SECS:-600}"

worker_id() { # 服务端校验 [a-z0-9-]{2,64}
  local id="${CLUSTER}-${AGENT_SLUG}"
  id=$(echo "$id" | tr '[:upper:]_' '[:lower:]-')
  echo "${id:0:64}"
}
WORKER_ID=$(worker_id)

log() { echo "$(date "+%Y-%m-%dT%H:%M:%S%z") $*"; }

# api: $1=path $2=body(可空) → REPLY_BODY(响应体)/REPLY_STATUS(HTTP 码)。
# 直接调用（不经命令替换——$() 子 shell 会丢副作用变量）。临时文件用 mktemp：
# 主循环 claim / 后台 heartbeat+renew 并发调用不可共用固定名（$$ 在子 shell
# 里不变、各自计数会交错——曾因此互相删除响应体）。
api() { # $1=path $2=body $3=额外头（\n 分隔，可空）
  local path="$1" body="${2:-}" tmp code args=() hdr
  tmp=$(mktemp /tmp/ir-pool.XXXXXX) || return 0
  code=$(mktemp /tmp/ir-poolc.XXXXXX) || { rm -f "$tmp"; return 0; }
  while IFS= read -r hdr; do args+=(-H "$hdr"); done < <(auth_headers)
  if [ -n "${3:-}" ]; then
    while IFS= read -r hdr; do [ -n "$hdr" ] && args+=(-H "$hdr"); done <<<"$3"
  fi
  if [ -n "$body" ]; then
    args+=(-H 'Content-Type: application/json' -d "$body")
  fi
  curl -sS -m 40 -o "$tmp" -w '%{http_code}' \
    -X POST "${IR_SERVER}/agent-api/v1${path}" "${args[@]}" > "$code" 2>/dev/null || true
  REPLY_STATUS=$(cat "$code" 2>/dev/null)
  REPLY_STATUS=${REPLY_STATUS:-000}
  REPLY_BODY=$(cat "$tmp" 2>/dev/null)
  rm -f "$tmp" "$code"
}

auth_headers() {
  printf 'Authorization: Bearer %s\nX-Agent-Slug: %s\n' "$AGENT_KEY" "$AGENT_SLUG"
  [ -n "$CLUSTER" ] && printf 'X-Agent-Cluster: %s\n' "$CLUSTER"
  return 0
}

# ---------------------------------------------------------------------------
# 原管线补丁：把 run-task.sh 的 dispatcher 上报替换为落盘，其余逐字节保留。
# 特征码校验对齐 deploy.sh 的部署习惯；源文件更新（mtime 更新）自动重打。
# ---------------------------------------------------------------------------
prepare_patched() {
  local dst="$POOL/run-task.sh"
  if [ -f "$dst" ] && grep -q "IR-PATCHED" "$dst" && [ "$RUN_TASK_ORIG" -nt "$dst" ]; then
    log "patch  源 run-task.sh 更新，重新打补丁"
    rm -f "$dst"
  fi
  if [ ! -f "$dst" ]; then
    sed -e 's|AGENT_KEY="${AGENT_KEY:?need nodek_ key}"|AGENT_KEY="${AGENT_KEY:-}"|' \
        -e 's|^post_result "\$payload"$|printf "%s" "$payload" > "$d/ir-result.json"  # IR-PATCHED|' \
        "$RUN_TASK_ORIG" > "$dst.tmp" || {
      log "FATAL  打补丁失败: $RUN_TASK_ORIG 不可读"; exit 1;
    }
    grep -q "IR-PATCHED" "$dst.tmp" || {
      log "FATAL  补丁未命中（原脚本上报行已变化），拒绝启动"; exit 1;
    }
    mv "$dst.tmp" "$dst"
    chmod +x "$dst"
    log "patch  已生成 IR-PATCHED run-task.sh at $dst"
  fi
}

# setsid（Linux util-linux）可用时任务进独立进程组，kill 树用负 pid；
# macOS 无 setsid，降级普通后台 + 直接 kill（本地测试用）。
START_BACKEND=setsid
command -v setsid >/dev/null 2>&1 || START_BACKEND=""

kill_tree() { # $1=pid（setsid 模式=组长）
  if [ -n "$START_BACKEND" ]; then
    kill -- -"$1" 2>/dev/null
    sleep 1
    kill -9 -- -"$1" 2>/dev/null
  else
    kill "$1" 2>/dev/null
    sleep 1
    kill -9 "$1" 2>/dev/null
  fi
}

mkdir -p "$POOL/runs" "$POOL/undelivered"
cd "$POOL" || exit 1
prepare_patched

running_workers=()
in_flight() {
  local n=0 p alive=()
  for p in "${running_workers[@]:-}"; do
    [ -n "$p" ] || continue
    if kill -0 "$p" 2>/dev/null; then n=$((n + 1)); alive+=("$p"); fi
  done
  running_workers=("${alive[@]:-}")
  echo "$n"
}

# report_ir_complete: $1=runId $2=payload_json（status/payload/error/usage/metrics）
# 幂等：重试 5 次 2/4/6/8s；全败落 undelivered/<runId>.json 供补投。
report_ir_complete() {
  local runId="$1" payload="$2" i
  local extra; extra=$(printf 'X-Agent-Run-ID: %s\nX-Agent-Worker-ID: %s' "$runId" "$WORKER_ID")
  for i in 1 2 3 4 5; do
    api "/complete" "$payload" "$extra"
    if [ "$REPLY_STATUS" = "200" ] || [ "$REPLY_STATUS" = "409" ] || [ "$REPLY_STATUS" = "404" ]; then
      log "complete $runId → $REPLY_STATUS"
      return 0
    fi
    [ "$i" = 5 ] || sleep $((i * 2))
  done
  printf '%s' "$payload" > "$POOL/undelivered/$runId.json"
  log "post   $runId 回报 5 次失败,已留档 undelivered/$runId.json"
}

deadline_epoch() { # RFC3339 → epoch（含时区）；失败回退 0=无墙钟。
  # 用 python3 解析（sec03 管线同款依赖）：date -d/-j 的时区语义在 GNU/BSD
  # 间不一致，UTC 值会被按本地时区误读导致假超时。
  [ -n "${1:-}" ] && python3 -c '
import sys
from datetime import datetime
try:
    print(int(datetime.fromisoformat(sys.argv[1].replace("Z", "+00:00")).timestamp()))
except Exception:
    print(0)
' "$1" || echo 0
}

# run_worker: $1=runId $2=renewAfterSecs $3=executionDeadline(rfc3339) $4=taskJsonFile
# 后台执行：起补丁版 run-task.sh（独立进程组）+ renew 循环 + 完成后转 IR complete。
run_worker() {
  local runId="$1" renewAfter="$2" deadline="$3" tf="$4"
  local d="$POOL/runs/$runId" slot=$((RANDOM % MAX_SLOTS + 1))
  mkdir -p "$d"
  if [ -n "$START_BACKEND" ]; then
    setsid nohup "$POOL/run-task.sh" "$runId" "$slot" <"$tf" >>"$POOL/pool.log" 2>&1 &
  else
    nohup "$POOL/run-task.sh" "$runId" "$slot" <"$tf" >>"$POOL/pool.log" 2>&1 &
  fi
  local pid=$!
  log "dispatch runId=$runId slot=$slot renewAfter=${renewAfter}s pid=$pid backend=${START_BACKEND:-plain}"

  local deadEpoch; deadEpoch=$(deadline_epoch "$deadline")
  local aborted=0 reason="" nextRenew=$renewAfter elapsed=0
  # 2s 粒度轮询：完成/取消即刻响应；每 renewAfter 间隔才发一次 renew。
  while kill -0 "$pid" 2>/dev/null; do
    if [ "$deadEpoch" -gt 0 ] && [ "$(date +%s)" -ge "$deadEpoch" ]; then
      aborted=1; reason="task_timeout"
      log "TIMEOUT runId=$runId 超 executionDeadline → 杀进程组"
      break
    fi
    [ -f "$d/done" ] && break
    sleep 2
    elapsed=$((elapsed + 2))
    if [ "$elapsed" -lt "$nextRenew" ]; then continue; fi
    nextRenew=$((nextRenew + renewAfter))
    api "/renew" ""
    local rbody="$REPLY_BODY"
    case "$REPLY_STATUS" in
      200)
        if jq -e '.abort == true' <<<"$rbody" >/dev/null 2>&1; then
          aborted=1; reason="aborted"
          log "ABORT  runId=$runId 平台请求取消 → 杀进程组"
          break
        fi
        ;;
      409)
        reason=$(jq -r '.code // "lease_lost"' <<<"$rbody" 2>/dev/null || echo lease_lost)
        aborted=1
        log "LOST   runId=$runId $reason → 放弃本地执行（结果不会被接受）"
        break
        ;;
      404)
        aborted=1; reason="run_not_found"
        break
        ;;
    esac
  done
  if [ "$aborted" = "1" ]; then
    kill_tree "$pid"
    [ "$reason" = "task_timeout" ] && touch "$d/done"
  fi
  # 等原管线收尾（done 由补丁版写入；abort 路径直接终局）
  local waited=0
  while [ ! -f "$d/done" ] && [ "$waited" -lt 20 ]; do
    sleep 1; waited=$((waited + 1))
  done
  rm -f "$d/attempt" "$d/last_slot" 2>/dev/null

  # 结果转换 → IR complete
  local payload
  if [ -f "$d/ir-result.json" ]; then
    local rstatus
    rstatus=$(jq -r '.status // "failed"' "$d/ir-result.json" 2>/dev/null)
    if [ "$rstatus" = "completed" ] || [ "$rstatus" = "succeeded" ]; then
      # usage 转换：原管线 BoundRunUsage(promptTokens/completionTokens/totalTokens)
      # → IR camelCase 计量契约；totalTokens 必填（RULE-BND-001，兜底 > 0）。
      payload=$(jq -n --argjson p "$(cat "$d/ir-result.json")" '
        {status:"succeeded",
         payload:(.p.result // .p),
         usage:{inputTokens:(.p.usage.promptTokens // .p.usage.inputTokens // 0),
                outputTokens:(.p.usage.completionTokens // .p.usage.outputTokens // 0),
                totalTokens:(.p.usage.totalTokens // ((.p.usage.promptTokens // 0) + (.p.usage.completionTokens // 0) + 1))},
         metrics:{durationMs:0}}')
    else
      local emsg
      emsg=$(jq -r '.error_message // "executor failed"' "$d/ir-result.json" 2>/dev/null)
      payload=$(jq -n --arg m "${emsg:0:500}" \
        '{status:"failed", error:{code:"executor_failed", message:$m}}')
    fi
  elif [ "$reason" = "aborted" ]; then
    payload='{"status":"aborted"}'
  else
    # 原管线启动即死且无结果：留给服务端租约超时重派（不回传，避免占用 attempts 语义）
    log "WARN   runId=$runId 无 ir-result.json（启动即死?）——交服务端租约超时处理"
    return 0
  fi
  report_ir_complete "$runId" "$payload"
  log "done   runId=$runId"
}

# heartbeat 独立循环：容量上报 + 服务端自适应间隔；401 即退出（密钥失效）。
run_heartbeat() {
  local interval=60
  while true; do
    local body rbody want
    body=$(jq -n --arg w "$WORKER_ID" --argjson f "$(in_flight)" --argjson c "$MAX_SLOTS" \
      --arg s "$AGENT_SLUG" \
      '{workerId:$w, inFlight:$f, capacity:$c, capabilities:{slugs:[$s], version:"agent-pool/1"}}')
    api "/heartbeat" "$body"
    if [ "$REPLY_STATUS" = "200" ]; then
      want=$(jq -r '.heartbeatIntervalSeconds // 60' <<<"$REPLY_BODY" 2>/dev/null)
      case "$want" in ''|*[!0-9]*) want=60 ;; esac
      [ "$want" -gt 3600 ] && want=3600
      interval=$want
    elif [ "$REPLY_STATUS" = "401" ]; then
      log "FATAL  heartbeat 401：密钥无效/被吊销，退出"
      exit 1
    fi
    sleep "$interval"
  done
}

run_heartbeat &
HEARTBEAT_PID=$!
trap 'kill "$HEARTBEAT_PID" 2>/dev/null' EXIT

log "start  worker=$WORKER_ID slug=$AGENT_SLUG slots=$MAX_SLOTS server=$IR_SERVER"
while true; do
  in_flight >/dev/null   # 顺带回收已退出的 worker pid
  local_free=$(in_flight)
  if [ "$local_free" -lt "$MAX_SLOTS" ]; then
    body=$(jq -n --arg w "$WORKER_ID" --argjson f "$local_free" --argjson c "$MAX_SLOTS" \
      --arg s "$AGENT_SLUG" \
      '{workerId:$w, maxWaitSeconds:20, inFlight:$f, capacity:$c, preferredSlugs:[$s]}')
    api "/claim" "$body"
    case "$REPLY_STATUS" in
      200)
        runId=$(jq -r '.runId' <<<"$REPLY_BODY" 2>/dev/null)
        renewAfter=$(jq -r '.renewAfterSeconds // 30' <<<"$REPLY_BODY" 2>/dev/null)
        deadline=$(jq -r '.executionDeadline // ""' <<<"$REPLY_BODY" 2>/dev/null)
        attempts=$(jq -r '.attempts // 1' <<<"$REPLY_BODY" 2>/dev/null)
        log "claim  runId=$runId attempts=$attempts"
        # IR input → dispatcher 任务格式（原 run-task.sh stdin 契约 {task_id,prompt,input}）：
        # taskBrief → prompt；params → input（与原 input.json/sec03 管线对接）。
        tf=$(mktemp "$POOL/runs/claim.XXXXXX.json")
        jq -n --arg id "$runId" --argjson in "$(jq -c '.input // {}' <<<"$REPLY_BODY")" \
          '{task_id:$id,
            prompt:(if ($in.taskBrief // "" | length) > 0 then $in.taskBrief else ($in | tostring) end),
            input:($in.params // {})}' > "$tf"
        run_worker "$runId" "$renewAfter" "$deadline" "$tf" &
        running_workers+=($!)
        ;;
      204) ;;      # 空队列：立即重新 claim（长轮询窗口内服务端持有）
      429) sleep 2 ;;
      401)
        log "FATAL  claim 401：密钥无效/被吊销/Scope 不符，退出"
        exit 1
        ;;
      *) sleep 1 ;;
    esac
  fi
  find "$POOL/runs" -mindepth 1 -maxdepth 1 -type d -mtime +3 -exec rm -rf {} + 2>/dev/null
  sleep 1
done
