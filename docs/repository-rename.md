# Repository rename verification — issue #5

Verified on 2026-09-09 for [it_telegram #5](https://github.com/shuoqiudi/it_telegram/issues/5), part of [it_manage #1247](https://github.com/shuoqiudi/it_manage/issues/1247).

## Identity and preflight

| Check | Before | After |
| --- | --- | --- |
| GitHub repository | `shuoqiudi/botmux` | `shuoqiudi/it_telegram` |
| Repository ID | `1360060767` / `R_kgDOURDhXw` | Identical |
| Default branch | `main` | `main` |
| Remote main at rename | `cfae7f856f9864a401d6220e8ca42dd8b70a19b0` | Identical |
| Local checkout | `/home/ecs-user/code/it_telegram` | Same independent checkout |
| Local origin | `git@github.com:shuoqiudi/botmux.git` | `git@github.com:shuoqiudi/it_telegram.git` |

Before mutation, the authenticated GitHub API returned 404 for the target name and `permissions.admin=true` for the original repository. The checkout was clean, with no stashes or unpushed commits; local HEAD matched remote main and the `it_manage` Gateway gitlink. The existing Gateway submodule was also clean and at the same commit. No existing directory or repository was overwritten.

The repository was renamed in place with `PATCH /repos/shuoqiudi/botmux` and `name=it_telegram`. No replacement repository was created. A before/after `git ls-remote` comparison matched every advertised ref, including branches, pull refs and tags. All six existing Issue/PR numeric IDs survived; issue #5 retained ID `5388209569`. The first post-rename list response briefly omitted #5/#6; direct lookups and a subsequent complete list confirmed both. The release lists were empty before and after.

The local checkout already had its own `.git` directory, with no superproject or object alternates. It is beside `/home/ecs-user/code/it_manage`, `it_caddy`, and `it_monitor`. Only its origin URL needed updating; development does not require entering the Gateway submodule.

## Provenance and compatibility

Telegram Gateway is the application name; `it_telegram` is the repository name. The upstream remains [skrashevich/BotMux](https://github.com/skrashevich/botmux). The Apache-2.0 [LICENSE](../LICENSE) and all original Git history are retained unchanged.

Both the fixed downstream baseline [`cfae7f8`](https://github.com/shuoqiudi/it_telegram/commit/cfae7f856f9864a401d6220e8ca42dd8b70a19b0) and verified upstream revision [`4819842`](https://github.com/shuoqiudi/it_telegram/commit/4819842ff90b4675d60b79eaf577a73d6740b6d8) were accessible through the renamed repository's API. The upstream revision was also present in the history fetched into a fresh bare repository through the old URL.

| Consumer / link | Verification |
| --- | --- |
| Old repository web URL | HTTP 301 to `https://github.com/shuoqiudi/it_telegram` |
| Old issue #5 web URL | HTTP 301 to the same issue number in `it_telegram`; old and new API routes return the same issue ID |
| Old HTTPS clone URL | A fresh bare repository fetched the exact gitlink SHA `cfae7f856f9864a401d6220e8ca42dd8b70a19b0` successfully |
| Old SSH origin URL | `git ls-remote git@github.com:shuoqiudi/botmux.git refs/heads/main` returned the same SHA |
| New SSH origin URL | All advertised refs match the pre-rename snapshot |
| New LICENSE and workflow links | GitHub API successfully resolved LICENSE, `test.yml`, and `docker.yml` |
| README / contribution / documentation navigation | Points to the maintained repository; source instructions clone it and build locally |

The Go module and imports remain `github.com/skrashevich/botmux`. Install this fork from its checkout with `go install .`; the upstream `go install ...@latest` command does not install downstream Gateway changes. Binary names, upstream release links, update-check source and upstream container examples remain compatible. README explicitly identifies upstream documentation and release information.

The Docker publication workflow previously derived `IMAGE_NAME` from `github.repository`, which would silently change future publication to `ghcr.io/shuoqiudi/it_telegram`. It now explicitly retains `shuoqiudi/botmux`. No image was built for publication or pushed, and registry publish permissions were not exercised by this rename verification.

## Existing consumers and delivery boundary

`it_manage/.gitmodules` still uses `https://github.com/shuoqiudi/botmux.git`, and `submodule/telegram_gateway` still records mode `160000` and commit `cfae7f856f9864a401d6220e8ca42dd8b70a19b0`. Its current packaging, source label and provenance URLs remain valid through the verified redirect. `it_manage/ee/telegram_gateway` remains the operational build/deploy entry point; asset migration belongs to [issue #6](https://github.com/shuoqiudi/it_telegram/issues/6).

No files in sibling repositories were edited. Existing `it_manage` changes in `CONTEXT.md`, `REPO_MAP.md`, `docs/specs/context/knowledge-index.md`, and its untracked ADR-0002/ADR-0003 were left intact. The Gateway gitlink, the `it_manage_shell` gitlink, deployment scripts and runtime configuration were retained. No deployment, running service mutation, image/container rename, network/domain/protocol change, or deployment-machine change was performed.

Do not create another repository at the old `shuoqiudi/botmux` name while consumers depend on its redirect. If compatibility needs repair, first restore access to the fixed SHA and verify a fresh fetch before changing/removing any consumer gitlink. A documentation/workflow revert does not undo the GitHub rename; reverting the remote name is a separate administrative operation and requires checking name availability and both URL consumers again.

## Reproducible checks

```bash
gh api repos/shuoqiudi/it_telegram --jq '{id,node_id,full_name,default_branch}'
gh issue view 5 --repo shuoqiudi/it_telegram
git remote -v
git rev-parse --git-dir --git-common-dir --show-superproject-working-tree
git ls-remote git@github.com:shuoqiudi/it_telegram.git
git ls-remote git@github.com:shuoqiudi/botmux.git refs/heads/main

# Fetch in an empty repository so an existing local object cannot mask failure.
verification_dir=$(mktemp -d)
git init --bare "$verification_dir"
git -C "$verification_dir" fetch https://github.com/shuoqiudi/botmux.git cfae7f856f9864a401d6220e8ca42dd8b70a19b0
git -C "$verification_dir" rev-parse FETCH_HEAD
git -C "$verification_dir" cat-file -t 4819842ff90b4675d60b79eaf577a73d6740b6d8
```

Validation used the existing local `it-manage-gateway-1238-tests:latest` build-stage image with Go 1.26.8, cached modules and `CGO_ENABLED=0` because the host has no Go installation. Only this checkout was mounted at `/src` (read-only); no sibling checkout was mounted. The disposable containers had no external network, a two-CPU limit, 2 GiB memory limit and `GOMAXPROCS=2`.

- `go build -p 1 -o /tmp/botmux .` and `go build -p 1 ./...`: passed.
- `go test -p 1 -parallel 1 ./tests -run "Test.*Version" -count=1`: passed.
- `go test -p 1 -parallel 1 -count=1 ./...`: passed across all packages.
- `docs.json` JSON parsing, Docker workflow YAML parsing and `git diff --check`: passed.
- Before/after `it_manage` working-tree diff and status comparisons: identical.

The opt-in real Redis test was skipped because no disposable Redis address was supplied. No live Telegram/Jenkins/DEV deployment smoke or registry publication was performed; this ticket changes repository identity and navigation only. No new business seam or name-only test was introduced.
