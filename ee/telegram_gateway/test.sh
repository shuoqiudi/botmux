#!/bin/sh
# Entire suite, serially, with Redis isolated from every existing service.
set -eu
if [ "$#" -ne 0 ]; then echo 'usage: test.sh' >&2; exit 2; fi
repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
exec 9>/tmp/it_manage_telegram_gateway_smoke.lock
flock -n 9 || { echo 'another Gateway test run owns the serial test lock' >&2; exit 2; }
revision=$(git -C "$repo_root" rev-parse HEAD)
test_image=it_manage_telegram_gateway_smoke:$revision
runtime_image=it_manage_telegram_gateway_smoke_runtime:$revision
"$repo_root/ee/telegram_gateway/build.sh" "$test_image" smoke-tests
"$repo_root/ee/telegram_gateway/build.sh" "$runtime_image"
redis_name=it-telegram-tests-$$
cleanup() { docker rm -fv "$redis_name" >/dev/null 2>&1 || true; }
trap cleanup EXIT HUP INT TERM
docker run -d --name "$redis_name" --publish 127.0.0.1::8080 \
    redis:7-alpine@sha256:ff02b58f971e7d7d156a1267e283fcbbeee91773b6aa36c49dac28ecfe28eadf \
    redis-server --appendonly yes --appendfsync always >/dev/null
attempt=0
until docker exec "$redis_name" redis-cli ping >/dev/null 2>&1; do
    attempt=$((attempt + 1)); [ "$attempt" -lt 30 ] || exit 1
    sleep 1
done
python3 -m unittest discover -s "$repo_root/tests/packaging" -v
python3 "$repo_root/tests/packaging/runtime_smoke.py" "$runtime_image" "$test_image" "$redis_name"
docker run --rm --network "container:$redis_name" --env GOMAXPROCS=2 "$test_image" \
    sh -ec 'CGO_ENABLED=0 go build -p 1 ./...;
    go test -p 1 -parallel 1 ./internal/gateway -run "^TestRedisQueueDurableReclaimAndDLQMove$" -count=1 -gateway-redis-test-addr=127.0.0.1:6379;
    go test -p 1 -parallel 1 ./... -count=1'
