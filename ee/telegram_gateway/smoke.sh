#!/bin/sh
set -eu

if [ "${1:-}" = "--list" ]; then
    printf '%s\n' management polling push-proxy bot-api-proxy
    exit 0
fi
if [ "$#" -ne 0 ]; then
    echo "usage: smoke.sh [--list]" >&2
    exit 2
fi

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
verified_revision=$(git -C "${repo_root}" rev-parse HEAD)
if [ -n "$(git -C "${repo_root}" status --porcelain --untracked-files=all)" ]; then
    echo "telegram gateway checkout has uncommitted changes" >&2
    exit 2
fi

exec 9>/tmp/it_manage_telegram_gateway_smoke.lock
if ! flock -n 9; then
    echo "another telegram gateway smoke run owns the serial test lock" >&2
    exit 2
fi

test_image=it_manage_telegram_gateway_smoke:${verified_revision}
"${repo_root}/ee/telegram_gateway/build.sh" "${test_image}" smoke-tests >/dev/null 2>&1

runtime_image=it_manage_telegram_gateway_smoke_runtime:${verified_revision}
"${repo_root}/ee/telegram_gateway/build.sh" "${runtime_image}" >/dev/null 2>&1

smoke_container=it-manage-telegram-gateway-smoke-$$
stop_management_smoke() {
    docker stop --time 2 "${smoke_container}" >/dev/null 2>&1 || true
}
trap stop_management_smoke EXIT HUP INT TERM

docker run --detach \
    --name "${smoke_container}" \
    --rm \
    --log-driver none \
    --read-only \
    --tmpfs /tmp:rw,noexec,nosuid,nodev,size=32m \
    --cap-drop ALL \
    --cap-add DAC_OVERRIDE \
    --cap-add SETGID \
    --cap-add SETUID \
    --security-opt no-new-privileges:true \
    --publish 127.0.0.1::8080 \
    "${runtime_image}" -demo >/dev/null

if ! docker run --rm "${test_image}" \
    go test -p 1 -parallel 1 . -count=1 -run '^TestResolveTelegramToken' >/dev/null 2>&1; then
    echo "FAIL management (token-file contract failed)" >&2
    exit 1
fi

management_port=$(docker port "${smoke_container}" 8080/tcp | awk -F: 'END {print $NF}')
attempt=0
until curl --fail --silent "http://127.0.0.1:${management_port}/api/health" >/dev/null \
    && curl --fail --silent "http://127.0.0.1:${management_port}/" | grep -q 'BotMux'; do
    attempt=$((attempt + 1))
    if [ "${attempt}" -ge 30 ]; then
        echo "FAIL management (runtime did not become healthy)" >&2
        exit 1
    fi
    sleep 1
done
stop_management_smoke
trap - EXIT HUP INT TERM
printf 'PASS management\n'

run_case() {
    case_name=$1
    test_pattern=$2
    if docker run --rm "${test_image}" \
        go test -p 1 -parallel 1 ./tests -count=1 -run "${test_pattern}" >/dev/null 2>&1; then
        printf 'PASS %s\n' "${case_name}"
        return 0
    fi
    printf 'FAIL %s (upstream output withheld to preserve the credential-safe smoke contract)\n' \
        "${case_name}" >&2
    return 1
}

run_case polling '^(TestE2E_Smoke|TestE2E_Errors|TestE2E_RestartWaitsForPriorPollingOwner)$'
run_case push-proxy '^(TestE2E_Updates_PushProxyDeliversWithoutSensitiveLogs|TestE2E_Updates_DoesNotSkipLaterBatchUpdatesAfterForwardFailure)$'
run_case bot-api-proxy '^(TestE2E_Callback_C03_SendMessageWithInlineKeyboard|TestE2E_Callback_C04_ProxyLogsRedactTokenAndChat)$'
