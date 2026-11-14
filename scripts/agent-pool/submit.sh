#!/usr/bin/env bash
# submit.sh —— cron/本地事件直接提交运行结果（POST /agent-api/v1/runs）。
# 用法：
#   submit.sh -slug my-agent -status succeeded -payload result.json [-input in.json] [-runid cron-20260101] [-code ERR_X]
#   ... | submit.sh -slug my-agent -payload -        # payload 从 stdin 读
# 定时任务包装示例（crontab）：
#   30 1 * * * /path/run-analysis.sh | ~/ir-pool/submit.sh -slug my-agent -payload - >> ~/ir-pool/cron.log 2>&1
set -u
IR_SERVER="${IR_SERVER:?need IR_SERVER}"
AGENT_SLUG_DEFAULT="${AGENT_SLUG:-}"
AGENT_KEY="${AGENT_KEY:?need agent signing key or cluster subkey}"
CLUSTER="${CLUSTER:-}"

slug="" status="succeeded" runId="" payloadFile="" inputFile="" errorCode=""
while getopts "s:p:i:r:c:" opt; do
  case "$opt" in
    s) slug="$OPTARG" ;;
    p) payloadFile="$OPTARG" ;;
    i) inputFile="$OPTARG" ;;
    r) runId="$OPTARG" ;;
    c) errorCode="$OPTARG" ;;
    *) echo "usage: submit.sh -slug S -status succeeded|failed -payload FILE|- [-input FILE] [-runid ID] [-code CODE]" >&2; exit 2 ;;
  esac
done
[ -n "$slug" ] || slug="$AGENT_SLUG_DEFAULT"
[ -n "$slug" ] || { echo "need -slug or AGENT_SLUG" >&2; exit 2; }

status="${status:-succeeded}"
[ "$status" = "succeeded" ] || [ "$status" = "failed" ] || { echo "-status must be succeeded|failed" >&2; exit 2; }

payload="{}"
if [ -n "$payloadFile" ]; then
  if [ "$payloadFile" = "-" ]; then
    payload=$(cat)
  else
    payload=$(cat "$payloadFile")
  fi
fi
input="null"
if [ -n "$inputFile" ]; then
  input=$(cat "$inputFile")
fi

body=$(jq -n --arg slug "$slug" --arg status "$status" --arg runId "$runId" \
  --argjson payload "$payload" --argjson input "$input" --arg code "$errorCode" \
  '{slug:$slug, status:$status, payload:$payload, input:$input} +
   (if $runId != "" then {runId:$runId} else {} end) +
   (if $code != "" then {error:{code:$code, message:"submitted by submit.sh"}} else {} end)')

args=(-H "Authorization: Bearer $AGENT_KEY" -H "X-Agent-Slug: $slug")
[ -n "$CLUSTER" ] && args+=(-H "X-Agent-Cluster: $CLUSTER")
args+=(-H 'Content-Type: application/json' -d "$body")

resp=$(curl -sS -m 30 -w '\n%{http_code}' -X POST "${IR_SERVER}/agent-api/v1/runs" "${args[@]}")
code=${resp##*$'\n'}
body_out=${resp%$'\n'*}
if [ "$code" = "200" ]; then
  echo "$body_out"
else
  echo "submit rejected: HTTP $code $body_out" >&2
  exit 1
fi
