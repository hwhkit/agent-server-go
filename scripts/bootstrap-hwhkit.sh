#!/bin/sh
# Materialize the unpublished hwhkit modules at the exact revision used by CI.
set -eu

if [ "$#" -ne 3 ]; then
    echo "usage: bootstrap-hwhkit.sh TARGET REPOSITORY FULL_GIT_SHA" >&2
    exit 2
fi

target=$1
repository=$2
expected_ref=$3

case "$target" in
    /*) ;;
    *) echo "错误: hwhkit target 必须是绝对路径" >&2; exit 2 ;;
esac
case "$target" in
    / | /.. | */../* | */..) echo "错误: hwhkit target 路径不安全" >&2; exit 2 ;;
esac
case "$expected_ref" in
    *[!0-9a-f]*) echo "错误: HWHKIT_REF 必须是小写十六进制 commit SHA" >&2; exit 2 ;;
esac
if [ "${#expected_ref}" -ne 40 ]; then
    echo "错误: HWHKIT_REF 必须是完整的 40 位 commit SHA" >&2
    exit 2
fi
case "$repository" in
    https://* | ssh://* | git@*:* | /*) ;;
    *) echo "错误: HWHKIT_REPOSITORY 必须是 HTTPS、SSH 或本地绝对路径" >&2; exit 2 ;;
esac

validate_checkout() {
    checkout=$1
    if ! git -C "$checkout" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
        echo "错误: $checkout 已存在但不是 Git checkout" >&2
        return 1
    fi
    actual=$(git -C "$checkout" rev-parse HEAD)
    if [ "$actual" != "$expected_ref" ]; then
        echo "错误: hwhkit revision 不一致（actual=$actual, expected=$expected_ref）" >&2
        echo "为避免覆盖本地工作，请手动处理 $checkout 后重试" >&2
        return 1
    fi
    if [ -n "$(git -C "$checkout" status --porcelain)" ]; then
        echo "错误: hwhkit checkout 有未提交修改，拒绝用于可复现构建" >&2
        return 1
    fi
    for module in buildinfo config core observability hwhkit; do
        if [ ! -f "$checkout/$module/go.mod" ]; then
            echo "错误: hwhkit checkout 缺少 $module/go.mod" >&2
            return 1
        fi
    done
}

if [ -e "$target" ]; then
    validate_checkout "$target"
    echo "hwhkit: $expected_ref ($target)"
    exit 0
fi

parent=$(dirname "$target")
mkdir -p "$parent"
lock=$target.bootstrap.lock
lock_owned=0
attempt=0
while ! mkdir "$lock" 2>/dev/null; do
    if [ -e "$target" ]; then
        validate_checkout "$target"
        echo "hwhkit: $expected_ref ($target)"
        exit 0
    fi
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 30 ]; then
        echo "错误: 等待 hwhkit bootstrap 锁超时: $lock" >&2
        exit 1
    fi
    sleep 1
done
lock_owned=1
stage=$(mktemp -d "$parent/.hwhkit-bootstrap.XXXXXXXXXX")
cleanup() {
    if [ -n "${stage:-}" ] && [ -d "$stage" ]; then
        rm -rf -- "$stage"
    fi
    if [ "${lock_owned:-0}" -eq 1 ]; then
        rmdir "$lock" 2>/dev/null || true
    fi
}
trap cleanup 0
trap 'exit 1' HUP INT TERM

if [ -e "$target" ]; then
    validate_checkout "$target"
    echo "hwhkit: $expected_ref ($target)"
    exit 0
fi

git -C "$stage" init -q
git -C "$stage" remote add origin "$repository"
git -C "$stage" sparse-checkout init --cone
git -C "$stage" sparse-checkout set buildinfo config core observability hwhkit
attempt=1
while ! git -C "$stage" -c http.version=HTTP/1.1 fetch -q --depth 1 origin "$expected_ref"; do
    if [ "$attempt" -ge 3 ]; then
        echo "错误: 拉取固定 hwhkit commit 失败（已尝试 $attempt 次）" >&2
        exit 1
    fi
    delay=$((attempt * 2))
    echo "警告: 拉取 hwhkit 失败，${delay}s 后重试（$attempt/3）" >&2
    sleep "$delay"
    attempt=$((attempt + 1))
done
git -C "$stage" checkout -q --detach FETCH_HEAD
validate_checkout "$stage"

mv "$stage" "$target"
stage=

echo "hwhkit: $expected_ref ($target)"
