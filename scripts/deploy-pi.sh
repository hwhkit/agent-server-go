#!/bin/sh
# Install only an explicitly pinned coding-agent package, never a mutable tag.
set -eu

if [ "$#" -ne 3 ]; then
    echo "usage: deploy-pi.sh HOST ENV_FILE PACKAGE@EXACT_VERSION" >&2
    exit 2
fi

host=$1
env_file=$2
package=$3

if ! printf '%s\n' "$package" | grep -Eq '^@earendil-works/pi-coding-agent@[0-9]+\.[0-9]+\.[0-9]+$'; then
    echo "错误: PI_NPM_PACKAGE 必须是 @earendil-works/pi-coding-agent@X.Y.Z 的精确版本" >&2
    exit 2
fi
case "$host" in
    '' | -* | *[!A-Za-z0-9_.@:-]*) echo "错误: REMOTE_HOST 包含不安全字符" >&2; exit 2 ;;
esac
case "$env_file" in
    /*) ;;
    *) echo "错误: REMOTE_ENV 必须是绝对路径" >&2; exit 2 ;;
esac
case "$env_file" in
    *[!A-Za-z0-9_./-]*) echo "错误: REMOTE_ENV 包含不安全字符" >&2; exit 2 ;;
esac

if [ "${DEPLOY_DRY_RUN:-0}" = "1" ]; then
    printf 'dry-run: install %s on %s and update %s\n' "$package" "$host" "$env_file"
    exit 0
fi

ssh "$host" "sh -s -- '$package' '$env_file'" <<'REMOTE'
set -eu
package=$1
env_file=$2

if ! command -v node >/dev/null 2>&1 || ! node -e '
    const [major, minor] = process.versions.node.split(".").map(Number);
    process.exit(major > 22 || (major === 22 && minor >= 19) ? 0 : 1);
'; then
    echo "错误: Pi coding-agent 需要 Node.js 22.19 或更新版本" >&2
    exit 1
fi
if ! command -v npm >/dev/null 2>&1; then
    echo "错误: 远端主机未安装 npm" >&2
    exit 1
fi
if [ ! -f "$env_file" ] || [ -L "$env_file" ]; then
    echo "错误: REMOTE_ENV 必须是已存在的普通文件，且不能是符号链接" >&2
    exit 1
fi

global_root=$(sudo npm root -g)
package_root=$(readlink -f "$global_root")/@earendil-works/pi-coding-agent
if command -v pi >/dev/null 2>&1; then
    pi_path=$(command -v pi)
    resolved=$(readlink -f "$pi_path")
    case "$resolved" in
        "$package_root"/*) ;;
        *) echo "错误: 现有 pi 命令属于其他包: $resolved" >&2; exit 1 ;;
    esac
fi

sudo npm install -g --ignore-scripts --engine-strict "$package"
package_json=$package_root/package.json
if [ ! -f "$package_json" ]; then
    echo "错误: 安装后未找到目标 Pi 包" >&2
    exit 1
fi
installed=$(node -e '
    const pkg = require(process.argv[1]);
    process.stdout.write(`${pkg.name}@${pkg.version}`);
' "$package_json")
if [ "$installed" != "$package" ]; then
    echo "错误: Pi 包身份或版本不匹配: $installed" >&2
    exit 1
fi
pi_path=$(command -v pi) || { echo "错误: 安装后找不到 pi 命令" >&2; exit 1; }
case "$pi_path" in
    /*) ;;
    *) echo "错误: pi 命令不是绝对路径: $pi_path" >&2; exit 1 ;;
esac
case "$pi_path" in
    *[!A-Za-z0-9_./-]*) echo "错误: pi 路径包含不安全字符" >&2; exit 1 ;;
esac
resolved=$(readlink -f "$pi_path")
case "$resolved" in
    "$package_root"/*) ;;
    *) echo "错误: pi 命令未指向已验证的包: $resolved" >&2; exit 1 ;;
esac

if sudo grep -q '^LOCAL_AGENT__PI_BINARY=' "$env_file"; then
    sudo sed -i "s|^LOCAL_AGENT__PI_BINARY=.*|LOCAL_AGENT__PI_BINARY=$pi_path|" "$env_file"
else
    printf '%s\n' "LOCAL_AGENT__PI_BINARY=$pi_path" | sudo tee -a "$env_file" >/dev/null
fi
echo "pi: $pi_path ($installed)"
REMOTE
