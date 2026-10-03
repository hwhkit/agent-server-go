#!/bin/sh
# Upload to a unique inode, verify it, then publish an immutable SHA-named stage.
set -eu

if [ "$#" -ne 4 ]; then
    echo "usage: deploy-upload.sh HOST BINARY ARTIFACT EXPECTED_SHA256" >&2
    exit 2
fi

host=$1
binary=$2
artifact=$3
expected_sha=$4

case "$host" in
    '' | -* | *[!A-Za-z0-9_.@:-]*) echo "错误: REMOTE_HOST 包含不安全字符" >&2; exit 2 ;;
esac
case "$binary" in
    /*) ;;
    *) echo "错误: REMOTE_BIN 必须是绝对路径" >&2; exit 2 ;;
esac
case "$binary" in
    *[!A-Za-z0-9_./-]*) echo "错误: REMOTE_BIN 包含不安全字符" >&2; exit 2 ;;
esac
case "$expected_sha" in
    *[!0-9a-f]*) echo "错误: SHA256 必须是小写十六进制" >&2; exit 2 ;;
esac
if [ "${#expected_sha}" -ne 64 ]; then
    echo "错误: SHA256 必须包含 64 个字符" >&2
    exit 2
fi
if [ ! -f "$artifact" ] || [ ! -s "$artifact" ]; then
    echo "错误: 部署产物不存在，请先运行 make deploy-build" >&2
    exit 1
fi

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
actual_sha=$(sh "$script_dir/file-sha256.sh" "$artifact")
if [ "$actual_sha" != "$expected_sha" ]; then
    echo "错误: 本地产物 SHA256 与期望值不一致" >&2
    exit 1
fi

if [ "${DEPLOY_DRY_RUN:-0}" = "1" ]; then
    printf 'dry-run: upload %s to %s:%s.new.%s\n' "$artifact" "$host" "$binary" "$expected_sha"
    exit 0
fi

upload=$(ssh "$host" "sh -s -- '$binary'" <<'REMOTE'
set -eu
binary=$1
for dependency in flock sha256sum mktemp; do
    if ! command -v "$dependency" >/dev/null 2>&1; then
        echo "错误: 远端主机缺少 $dependency" >&2
        exit 1
    fi
done
directory=${binary%/*}
if [ ! -d "$directory" ]; then
    echo "错误: 远端二进制目录不存在: $directory" >&2
    exit 1
fi
mktemp "$binary.upload.XXXXXXXXXX"
REMOTE
)
case "$upload" in
    "$binary".upload.??????????) ;;
    *) echo "错误: 远端返回了无效的上传暂存路径" >&2; exit 1 ;;
esac
case "$upload" in
    *[!A-Za-z0-9_./-]*) echo "错误: 上传暂存路径包含不安全字符" >&2; exit 1 ;;
esac

cleanup() {
    if ! ssh "$host" "rm -f -- '$upload'" </dev/null >/dev/null 2>&1; then
        echo "警告: 无法清理上传暂存文件 $upload" >&2
    fi
}
trap cleanup 0
trap 'exit 1' HUP INT TERM

scp -- "$artifact" "$host:$upload"
ssh "$host" "sh -s -- '$binary' '$upload' '$expected_sha'" <<'REMOTE'
set -eu
binary=$1
upload=$2
expected_sha=$3
exec 9>"$binary.deploy.lock"
if ! flock -n 9; then
    echo "错误: 另一部署正在替换或重启服务" >&2
    exit 1
fi
if [ ! -s "$upload" ]; then
    echo "错误: 上传暂存文件为空" >&2
    exit 1
fi
actual=$(sha256sum "$upload")
actual=${actual%% *}
if [ "$actual" != "$expected_sha" ]; then
    echo "错误: 上传二进制与本地产物 SHA256 不一致" >&2
    exit 1
fi
chmod 0755 "$upload"
staged=$binary.new.$expected_sha
if [ -e "$staged" ]; then
    staged_sha=$(sha256sum "$staged")
    staged_sha=${staged_sha%% *}
    if [ "$staged_sha" != "$expected_sha" ]; then
        echo "错误: 同名暂存产物 SHA256 不一致" >&2
        exit 1
    fi
    rm -f -- "$upload"
else
    mv "$upload" "$staged"
fi
REMOTE
