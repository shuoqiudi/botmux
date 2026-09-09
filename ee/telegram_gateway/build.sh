#!/bin/sh
set -eu

if [ "$#" -gt 2 ]; then
    echo 'usage: build.sh [image-ref [runtime|smoke-tests]]' >&2
    exit 2
fi
repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
image_ref=${1:-it_manage_telegram_gateway:dev}
target=${2:-runtime}
case "$target" in runtime|smoke-tests) ;; *) echo 'invalid build target' >&2; exit 2;; esac
if [ -n "$(git -C "$repo_root" status --porcelain --untracked-files=all)" ]; then
    echo 'commit changes before building a traceable Gateway image' >&2
    exit 2
fi
revision=$(git -C "$repo_root" rev-parse HEAD)
build_date=$(git -C "$repo_root" show -s --format=%cI HEAD)
exec docker build --file "$repo_root/Dockerfile" --target "$target" \
    --build-arg "COMMIT=$revision" --build-arg "VERSION=$revision" \
    --build-arg "BUILD_DATE=$build_date" --tag "$image_ref" "$repo_root"
