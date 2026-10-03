#!/bin/sh
set -eu

if [ "$#" -ne 3 ]; then
    echo "usage: remote-service.sh status|logs|logs-follow HOST SERVICE" >&2
    exit 2
fi

action=$1
host=$2
service=$3

case "$action" in
    status | logs | logs-follow) ;;
    *) echo "错误: 未知远程服务操作: $action" >&2; exit 2 ;;
esac
case "$host" in
    '' | -* | *[!A-Za-z0-9_.@:-]*) echo "错误: REMOTE_HOST 包含不安全字符" >&2; exit 2 ;;
esac
case "$service" in
    '' | -* | *[!A-Za-z0-9_.@-]*) echo "错误: REMOTE_SERVICE 包含不安全字符" >&2; exit 2 ;;
esac

if [ "${REMOTE_OP_DRY_RUN:-0}" = "1" ]; then
    printf 'dry-run: %s %s on %s\n' "$action" "$service" "$host"
    exit 0
fi

case "$action" in
    status)
        ssh "$host" "rc=0; systemctl is-active --quiet '$service' || rc=\$?; systemctl status '$service' --no-pager -l | head -15; exit \$rc"
        ;;
    logs)
        ssh "$host" "sudo journalctl -u '$service' -n 50 --no-pager"
        ;;
    logs-follow)
        ssh "$host" "sudo journalctl -u '$service' -f --no-pager"
        ;;
esac
