#!/bin/sh
set -eu

token_file=/run/secrets/telegram_bot_token
runtime_token_file=/tmp/telegram_bot_token
if [ -e "${token_file}" ]; then
    if [ ! -f "${token_file}" ] || [ ! -r "${token_file}" ]; then
        echo "telegram gateway token file is not readable" >&2
        exit 2
    fi
    rm -f "${runtime_token_file}"
    if ! su-exec telegram-gateway:telegram-gateway \
        sh -c 'umask 077; cat > "$1"' sh "${runtime_token_file}" \
        < "${token_file}"; then
        echo "telegram gateway token file could not be staged" >&2
        exit 2
    fi
    set -- -token-file "${runtime_token_file}" "$@"
fi

adapter_file=/run/secrets/adapter_config
if [ -e "${adapter_file}" ]; then
    runtime_adapter_file=/tmp/adapter_config
    if ! su-exec telegram-gateway:telegram-gateway \
        sh -c 'umask 077; cat > "$1"' sh "${runtime_adapter_file}" \
        < "${adapter_file}"; then
        echo "telegram gateway adapter config could not be staged" >&2
        exit 2
    fi
    set -- -adapter-config-file "${runtime_adapter_file}" "$@"
fi

exec su-exec telegram-gateway:telegram-gateway \
    /usr/local/bin/telegram-gateway "$@"
