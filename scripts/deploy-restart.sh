#!/bin/sh
# Atomically activate a staged binary, prove readiness and running provenance,
# then restore the previous binary automatically when rollout validation fails.
set -eu

if [ "$#" -ne 7 ]; then
    echo "usage: deploy-restart.sh HOST SERVICE BINARY HEALTH_URL ATTEMPTS INTERVAL EXPECTED_SHA256" >&2
    exit 2
fi

host=$1
service=$2
binary=$3
health_url=$4
attempts=$5
interval=$6
expected_sha=$7

case "$host" in
    '' | -* | *[!A-Za-z0-9_.@:-]*) echo "错误: REMOTE_HOST 包含不安全字符" >&2; exit 2 ;;
esac
case "$service" in
    '' | -* | *[!A-Za-z0-9_.@-]*) echo "错误: REMOTE_SERVICE 包含不安全字符" >&2; exit 2 ;;
esac
case "$binary" in
    /*) ;;
    *) echo "错误: REMOTE_BIN 必须是绝对路径" >&2; exit 2 ;;
esac
case "$binary" in
    *[!A-Za-z0-9_./-]*) echo "错误: REMOTE_BIN 包含不安全字符" >&2; exit 2 ;;
esac
case "$health_url" in
    http://* | https://*) ;;
    *) echo "错误: REMOTE_HEALTH_URL 必须是 HTTP(S) URL" >&2; exit 2 ;;
esac
case "$health_url" in
    *[!A-Za-z0-9._~:/?%=\&-]*) echo "错误: REMOTE_HEALTH_URL 包含不安全字符" >&2; exit 2 ;;
esac
case "$attempts" in
    '' | *[!0-9]*) echo "错误: REMOTE_HEALTH_ATTEMPTS 必须是整数" >&2; exit 2 ;;
esac
case "$interval" in
    '' | *[!0-9]*) echo "错误: REMOTE_HEALTH_INTERVAL 必须是整数" >&2; exit 2 ;;
esac
if [ "$attempts" -lt 1 ] || [ "$attempts" -gt 60 ]; then
    echo "错误: REMOTE_HEALTH_ATTEMPTS 必须介于 1 与 60" >&2
    exit 2
fi
if [ "$interval" -gt 60 ]; then
    echo "错误: REMOTE_HEALTH_INTERVAL 不得超过 60 秒" >&2
    exit 2
fi
case "$expected_sha" in
    *[!0-9a-f]*) echo "错误: SHA256 必须是小写十六进制" >&2; exit 2 ;;
esac
if [ "${#expected_sha}" -ne 64 ]; then
    echo "错误: SHA256 必须包含 64 个字符" >&2
    exit 2
fi

if [ "${DEPLOY_DRY_RUN:-0}" = "1" ]; then
    printf 'dry-run: activate %s.new.%s on %s; service=%s health=%s\n' \
        "$binary" "$expected_sha" "$host" "$service" "$health_url"
    exit 0
fi

ssh "$host" "sh -s -- '$service' '$binary' '$health_url' '$attempts' '$interval' '$expected_sha'" <<'REMOTE'
set -eu
service=$1
binary=$2
health_url=$3
attempts=$4
interval=$5
expected_sha=$6

for dependency in curl flock sha256sum sudo systemctl; do
    if ! command -v "$dependency" >/dev/null 2>&1; then
        echo "错误: 远端主机缺少 $dependency" >&2
        exit 1
    fi
done

exec 9>"$binary.deploy.lock"
if ! flock -n 9; then
    echo "错误: 另一部署正在替换或重启服务" >&2
    exit 1
fi

file_sha() {
    value=$(sha256sum "$1")
    printf '%s\n' "${value%% *}"
}

running_matches() {
    pid=$(systemctl show "$service" -p MainPID --value 2>/dev/null) || return 1
    case "$pid" in
        '' | 0 | *[!0-9]*) return 1 ;;
    esac
	# The service commonly runs as root or a dedicated account. Reading another
	# UID's /proc/<pid>/exe is blocked by ptrace policy on hardened hosts, so use
	# the same narrowly scoped sudo boundary already required for systemctl.
	running_value=$(sudo sha256sum "/proc/$pid/exe" 2>/dev/null) || return 1
	running_sha=${running_value%% *}
    [ "$running_sha" = "$expected_sha" ]
}

healthy() {
    attempt=1
    while [ "$attempt" -le "$attempts" ]; do
        if systemctl is-active --quiet "$service" \
            && running_matches \
            && curl -fsS --max-time 5 "$health_url" >/dev/null; then
            return 0
        fi
        if [ "$attempt" -lt "$attempts" ]; then
            sleep "$interval"
        fi
        attempt=$((attempt + 1))
    done
    return 1
}

staged=$binary.new.$expected_sha
activated=0
had_previous=0
if [ -e "$staged" ]; then
    if [ ! -s "$staged" ] || [ "$(file_sha "$staged")" != "$expected_sha" ]; then
        echo "错误: 暂存二进制 SHA256 不一致，拒绝替换" >&2
        exit 1
    fi
    if [ -f "$binary" ]; then
        cp -p "$binary" "$binary.previous"
        had_previous=1
    fi
    chmod 0755 "$staged"
    mv "$staged" "$binary"
    activated=1
elif [ ! -s "$binary" ] || [ "$(file_sha "$binary")" != "$expected_sha" ]; then
    echo "错误: 暂存与当前二进制都不匹配期望 SHA256，拒绝重启" >&2
    exit 1
fi

if sudo systemctl restart "$service" && healthy; then
    systemctl is-active "$service"
    exit 0
fi

echo "错误: 目标版本未通过服务、运行产物或就绪检查" >&2
if [ "$activated" -eq 1 ] && [ "$had_previous" -eq 1 ] && [ -s "$binary.previous" ]; then
    expected_sha=$(file_sha "$binary.previous")
    mv "$binary.previous" "$binary"
    if sudo systemctl restart "$service" && healthy; then
        echo "已恢复上一版本" >&2
    else
        echo "警告: 上一版本也未通过健康检查" >&2
    fi
elif [ "$activated" -eq 1 ]; then
    echo "警告: 未找到上一版本二进制，无法回滚" >&2
fi
exit 1
REMOTE
