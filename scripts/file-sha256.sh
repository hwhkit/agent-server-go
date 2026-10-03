#!/bin/sh
set -eu

if [ "$#" -ne 1 ]; then
    echo "usage: file-sha256.sh FILE" >&2
    exit 2
fi

file=$1
if [ ! -f "$file" ] || [ ! -s "$file" ]; then
    echo "错误: 文件不存在或为空: $file" >&2
    exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$file" | awk '{print $1}'
elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$file" | awk '{print $1}'
else
    echo "错误: 缺少 sha256sum 或 shasum" >&2
    exit 1
fi
