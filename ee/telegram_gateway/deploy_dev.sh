#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
compose_file="${repo_root}/ee/telegram_gateway/compose.yaml"
secret_file="${repo_root}/ee/telegram_gateway/secrets/telegram_bot_token"
image_ref="it_manage_telegram_gateway:dev"
remote_dir="/opt/telegram_gateway"
remote_secret="${remote_dir}/secrets/telegram_bot_token"
remote_host="${WINDMILL_SSH_USER:-}@${WINDMILL_SSH_IP:-}"
container_name="it-manage-telegram-gateway-dev"

fail() {
    printf 'ERROR: %s\n' "$1" >&2
    exit 1
}

[ -n "${WINDMILL_SSH_USER:-}" ] || fail "WINDMILL_SSH_USER is required"
[ -n "${WINDMILL_SSH_IP:-}" ] || fail "WINDMILL_SSH_IP is required"
[ -s "${secret_file}" ] || fail "dedicated DEV Bot token file is missing or empty"
[ "$(stat -c '%a' "${secret_file}")" = "600" ] || fail "DEV Bot token file mode must be 600"
[ "$(awk 'END {print NR}' "${secret_file}")" = "1" ] || fail "DEV Bot token file must contain exactly one line"

exec 9>/tmp/it_manage_telegram_gateway_deploy_dev.lock
flock -n 9 || fail "another Telegram Gateway DEV deployment is running"

local_container_uses_token() {
    local running_id="$1"
    local configured_values
    local inspect_status
    local mount_kind
    local mount_source

    if ! configured_values="$(docker inspect "${running_id}" --format '{{range .Config.Env}}{{println .}}{{end}}{{range .Config.Cmd}}{{println .}}{{end}}{{range .Config.Entrypoint}}{{println .}}{{end}}')"; then
        return 2
    fi
    if grep -F -f "${secret_file}" -q <<<"${configured_values}"; then
        return 0
    else
        inspect_status=$?
    fi
    [ "${inspect_status}" -eq 1 ] || return 2

    if ! configured_values="$(docker inspect "${running_id}" --format '{{range .Mounts}}{{printf "%s\t%s\n" .Type .Source}}{{end}}')"; then
        return 2
    fi
    while IFS=$'\t' read -r mount_kind mount_source; do
        [ "${mount_kind}" = "bind" ] || continue
        [ -e "${mount_source}" ] || return 2
        [ -f "${mount_source}" ] || continue
        if cmp -s "${mount_source}" "${secret_file}"; then
            return 0
        else
            inspect_status=$?
        fi
        [ "${inspect_status}" -eq 1 ] || return 2
    done <<<"${configured_values}"
    return 1
}

if ! running_ids="$(docker ps -q)"; then
    fail "cannot list local containers for token ownership inspection"
fi
while IFS= read -r running_id; do
    [ -n "${running_id}" ] || continue
    running_name="$(docker inspect "${running_id}" --format '{{.Name}}' | sed 's#^/##')"
    [ "${running_name}" = "${container_name}" ] && continue
    if local_container_uses_token "${running_id}"; then
        fail "another local container is a configured token owner: ${running_name}"
    else
        inspect_status=$?
        [ "${inspect_status}" -eq 1 ] \
            || fail "cannot safely inspect local container token ownership: ${running_name}"
    fi
done <<<"${running_ids}"

"${repo_root}/ee/telegram_gateway/build.sh" "${image_ref}"

ssh -o BatchMode=yes "${remote_host}" bash -s -- \
    "${remote_dir}" "${container_name}" <<'REMOTE'
set -euo pipefail
remote_dir="$1"
container_name="$2"

if docker version >/dev/null 2>&1; then
    docker_cmd=(docker)
elif sudo -n docker version >/dev/null 2>&1; then
    docker_cmd=(sudo -n docker)
else
    echo "ERROR: remote Docker is unavailable" >&2
    exit 1
fi

if ss -ltn 2>/dev/null | awk 'NR > 1 {print $4}' | grep -Eq '(^|:)18081$'; then
    if ! "${docker_cmd[@]}" ps --format '{{.Names}}' | grep -qx "${container_name}"; then
        echo "ERROR: remote port 18081 is occupied by another service" >&2
        exit 1
    fi
fi

sudo -n install -d -m 0755 "${remote_dir}"
sudo -n install -d -m 0700 "${remote_dir}/secrets"
REMOTE

ssh -o BatchMode=yes "${remote_host}" \
    "sudo -n tee '${remote_dir}/compose.yaml' >/dev/null && sudo -n chmod 0644 '${remote_dir}/compose.yaml'" \
    < "${compose_file}"
ssh -o BatchMode=yes "${remote_host}" \
    "sudo -n sh -c 'umask 077; cat > \"\$1\"; chmod 600 \"\$1\"' sh '${remote_secret}'" \
    < "${secret_file}"

docker save "${image_ref}" | gzip -1 | \
    ssh -o BatchMode=yes "${remote_host}" \
        'if docker version >/dev/null 2>&1; then gzip -d | docker load >/dev/null; else gzip -d | sudo -n docker load >/dev/null; fi'

# The dedicated Bot must have one polling owner during the host transition.
docker compose -f "${compose_file}" stop telegram_gateway >/dev/null

ssh -o BatchMode=yes "${remote_host}" bash -s -- \
    "${remote_dir}" "${remote_secret}" "${container_name}" <<'REMOTE'
set -euo pipefail
remote_dir="$1"
remote_secret="$2"
container_name="$3"

if docker version >/dev/null 2>&1; then
    docker_cmd=(docker)
else
    docker_cmd=(sudo -n docker)
fi

cd "${remote_dir}"

remote_container_uses_token() {
    local running_id="$1"
    local configured_values
    local inspect_status
    local mount_kind
    local mount_source

    if ! configured_values="$("${docker_cmd[@]}" inspect "${running_id}" --format '{{range .Config.Env}}{{println .}}{{end}}{{range .Config.Cmd}}{{println .}}{{end}}{{range .Config.Entrypoint}}{{println .}}{{end}}')"; then
        return 2
    fi
    if sudo -n grep -F -f "${remote_secret}" -q <<<"${configured_values}"; then
        return 0
    else
        inspect_status=$?
    fi
    [ "${inspect_status}" -eq 1 ] || return 2

    if ! configured_values="$("${docker_cmd[@]}" inspect "${running_id}" --format '{{range .Mounts}}{{printf "%s\t%s\n" .Type .Source}}{{end}}')"; then
        return 2
    fi
    while IFS=$'\t' read -r mount_kind mount_source; do
        [ "${mount_kind}" = "bind" ] || continue
        sudo -n test -e "${mount_source}" || return 2
        sudo -n test -f "${mount_source}" || continue
        if sudo -n cmp -s "${mount_source}" "${remote_secret}"; then
            return 0
        else
            inspect_status=$?
        fi
        [ "${inspect_status}" -eq 1 ] || return 2
    done <<<"${configured_values}"
    return 1
}

if ! running_ids="$("${docker_cmd[@]}" ps -q)"; then
    echo "ERROR: cannot list remote containers for token ownership inspection" >&2
    exit 1
fi
while IFS= read -r running_id; do
    [ -n "${running_id}" ] || continue
    running_name="$("${docker_cmd[@]}" inspect "${running_id}" --format '{{.Name}}' | sed 's#^/##')"
    [ "${running_name}" = "${container_name}" ] && continue
    if remote_container_uses_token "${running_id}"; then
        echo "ERROR: another remote container is a configured token owner: ${running_name}" >&2
        exit 1
    else
        inspect_status=$?
        if [ "${inspect_status}" -ne 1 ]; then
            echo "ERROR: cannot safely inspect remote container token ownership: ${running_name}" >&2
            exit 1
        fi
    fi
done <<<"${running_ids}"

"${docker_cmd[@]}" compose -f compose.yaml up -d --no-build telegram_gateway

attempt=0
until health="$("${docker_cmd[@]}" inspect "${container_name}" --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' 2>/dev/null)" \
    && [ "${health}" = "healthy" ]; do
    attempt=$((attempt + 1))
    if [ "${attempt}" -ge 45 ]; then
        printf 'ERROR: remote Gateway health=%s\n' "${health:-missing}" >&2
        exit 1
    fi
    sleep 1
done

# Observe at least one complete long-poll interval after the ownership switch.
sleep 35

"${docker_cmd[@]}" exec "${container_name}" sh -c '
    process_uid=$(sed -n "s/^Uid:[[:space:]]*\([0-9]*\).*/\1/p" /proc/1/status)
    effective_caps=$(sed -n "s/^CapEff:[[:space:]]*//p" /proc/1/status)
    token_uid=$(stat -c "%u" /tmp/telegram_bot_token)
    token_mode=$(stat -c "%a" /tmp/telegram_bot_token)
    [ "$process_uid" != "0" ]
    [ "$effective_caps" = "0000000000000000" ]
    [ "$token_uid" = "$process_uid" ]
    [ "$token_mode" = "600" ]
    wget -qO- http://127.0.0.1:8080/api/health | grep -q "\"status\":\"ok\""
'

if "${docker_cmd[@]}" logs "${container_name}" 2>&1 | sudo -n grep -F -f "${remote_secret}" -q; then
    echo "ERROR: remote Gateway logs contain the Bot token" >&2
    exit 1
fi
if "${docker_cmd[@]}" logs "${container_name}" 2>&1 | grep -q 'polling ownership conflict'; then
    echo "ERROR: remote Gateway is not the sole polling owner" >&2
    exit 1
fi
if ! "${docker_cmd[@]}" logs "${container_name}" 2>&1 | grep -q 'polling mode'; then
    echo "ERROR: remote Gateway polling mode was not observed" >&2
    exit 1
fi

printf 'GATEWAY_DEV_DEPLOY_STATUS=healthy host=%s container=%s\n' "$(hostname)" "${container_name}"
REMOTE
