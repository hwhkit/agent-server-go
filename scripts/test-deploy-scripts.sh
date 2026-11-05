#!/bin/sh
# Offline contract tests for deployment validation and dry-run paths.
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
project_root=$(dirname "$script_dir")
tmp=$(mktemp -d "${TMPDIR:-/tmp}/ir-go-deploy-test.XXXXXXXXXX")
cleanup() {
    rm -rf -- "$tmp"
}
trap cleanup 0
trap 'exit 1' HUP INT TERM

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

expect_failure() {
    if "$@" >/dev/null 2>&1; then
        fail "command unexpectedly succeeded: $*"
    fi
}

artifact=$tmp/ir-agent-server
printf 'abc' > "$artifact"
sha=$(sh "$script_dir/file-sha256.sh" "$artifact")
expected=ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad
[ "$sha" = "$expected" ] || fail "portable SHA256 helper returned $sha"

DEPLOY_DRY_RUN=1 sh "$script_dir/deploy-upload.sh" \
    deploy@example.test /opt/ir/bin/ir-agent-server "$artifact" "$sha" >/dev/null
DEPLOY_DRY_RUN=1 sh "$script_dir/deploy-restart.sh" \
    deploy@example.test ir-agent-server-go /opt/ir/bin/ir-agent-server \
    http://127.0.0.1:8080/health/ready 3 0 "$sha" >/dev/null
DEPLOY_DRY_RUN=1 sh "$script_dir/deploy-pi.sh" \
    deploy@example.test /opt/ir/env.conf \
    @earendil-works/pi-coding-agent@1.2.3 >/dev/null
REMOTE_OP_DRY_RUN=1 sh "$script_dir/remote-service.sh" \
    status deploy@example.test ir-agent-server-go >/dev/null

# Guard the Makefile-to-wrapper argument contract: this catches accidentally
# reverting to the old `ssh ... < deploy-restart.sh` calling convention.
make -s -C "$project_root" deploy-restart \
    DEPLOY_DRY_RUN=1 \
    REMOTE_HOST=deploy@example.test \
    REMOTE_SERVICE=ir-agent-server-go \
    REMOTE_BIN=/opt/ir/bin/ir-agent-server \
    REMOTE_HEALTH_URL=http://127.0.0.1:8080/health/ready \
    REMOTE_HEALTH_ATTEMPTS=3 \
    REMOTE_HEALTH_INTERVAL=0 \
    EXPECTED_SHA256="$sha" >/dev/null

# A fresh checkout keeps shell scripts non-executable. The Makefile must
# calculate its default checksum through sh without an explicit override.
default_checksum_output=$(make -s -C "$project_root" deploy-restart \
    DEPLOY_DRY_RUN=1 \
    DEPLOY_ARTIFACT="$artifact" \
    REMOTE_HOST=deploy@example.test \
    REMOTE_SERVICE=ir-agent-server-go \
    REMOTE_BIN=/opt/ir/bin/ir-agent-server \
    REMOTE_HEALTH_URL=http://127.0.0.1:8080/health/ready \
    REMOTE_HEALTH_ATTEMPTS=3 \
    REMOTE_HEALTH_INTERVAL=0)
printf '%s\n' "$default_checksum_output" | grep -F ".new.$sha" >/dev/null ||
    fail "Makefile did not calculate the default artifact checksum"

# Cross-UID services require privileged access to /proc/<pid>/exe; guard the
# provenance check from regressing to an unprivileged sha256sum call.
grep -F 'sudo sha256sum "/proc/$pid/exe"' "$script_dir/deploy-restart.sh" >/dev/null ||
    fail "running executable hash must use the scoped sudo boundary"

expect_failure env DEPLOY_DRY_RUN=1 sh "$script_dir/deploy-upload.sh" \
    -unsafe /opt/ir/bin/ir-agent-server "$artifact" "$sha"
expect_failure env DEPLOY_DRY_RUN=1 sh "$script_dir/deploy-upload.sh" \
    deploy@example.test relative/path "$artifact" "$sha"
expect_failure env DEPLOY_DRY_RUN=1 sh "$script_dir/deploy-upload.sh" \
    deploy@example.test /opt/ir/bin/ir-agent-server "$artifact" \
    aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
expect_failure env DEPLOY_DRY_RUN=1 sh "$script_dir/deploy-restart.sh" \
    deploy@example.test 'bad;service' /opt/ir/bin/ir-agent-server \
    http://127.0.0.1:8080/health/ready 3 0 "$sha"
expect_failure env DEPLOY_DRY_RUN=1 sh "$script_dir/deploy-pi.sh" \
    deploy@example.test /opt/ir/env.conf @earendil-works/pi-coding-agent@latest
expect_failure env REMOTE_OP_DRY_RUN=1 sh "$script_dir/remote-service.sh" \
    status deploy@example.test 'bad;service'

# Exercise the hwhkit bootstrap without network access.
source_repo=$tmp/hwhkit-source
mkdir -p "$source_repo"
git -C "$source_repo" init -q
git -C "$source_repo" config user.name test
git -C "$source_repo" config user.email test@example.invalid
for module in buildinfo config core observability hwhkit; do
    mkdir -p "$source_repo/$module"
    printf 'module example.invalid/%s\n\ngo 1.25.0\n' "$module" > "$source_repo/$module/go.mod"
done
git -C "$source_repo" add .
git -C "$source_repo" commit -q -m fixture
ref=$(git -C "$source_repo" rev-parse HEAD)
checkout=$tmp/hwhkit-checkout
sh "$script_dir/bootstrap-hwhkit.sh" "$checkout" "$source_repo" "$ref" >/dev/null
sh "$script_dir/bootstrap-hwhkit.sh" "$checkout" "$source_repo" "$ref" >/dev/null
[ "$(git -C "$checkout" rev-parse HEAD)" = "$ref" ] || fail "bootstrap revision mismatch"
printf '\n// dirty\n' >> "$checkout/buildinfo/go.mod"
expect_failure sh "$script_dir/bootstrap-hwhkit.sh" "$checkout" "$source_repo" "$ref"

echo "deployment script tests: ok"
