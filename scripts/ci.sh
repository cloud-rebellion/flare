#!/usr/bin/env bash
# Every check CI runs, in one script you can also run yourself:
#
#   bash scripts/ci.sh
#
# Run it before opening a pull request and CI will not tell you anything new.
#
# WHY THE LOGIC LIVES HERE AND NOT IN .github/workflows/ci.yml
#
# Two reasons, and the second is not obvious.
#
# 1. A contributor can run it. Checks that only exist inside a workflow file are
#    checks you cannot reproduce locally, so you discover them after pushing.
#
# 2. It keeps .github/workflows/ci.yml STABLE, which is what makes automated
#    publishing possible at all. This repository is a filtered mirror, published by
#    a CI job authenticated with a repository deploy key. GitHub does not allow a
#    deploy key (or an OAuth token without the `workflow` scope) to create or update
#    anything under .github/workflows/. So every time a CI step changed, the publish
#    was rejected at the final push, after every gate had already passed, with an
#    error that read like a mystery. With the steps in this script, changing a check
#    touches scripts/ci.sh (an ordinary file the deploy key may write) and the
#    workflow file stays untouched, so the publish goes through.
#
# The consequence to remember: editing .github/workflows/ci.yml is now a rare,
# deliberate act (an action version bump, a trigger change) that needs a push from a
# credential with the `workflow` scope. Editing THIS file needs nothing special.
set -uo pipefail

cd "$(git rev-parse --show-toplevel 2>/dev/null)" 2>/dev/null || true
[ -f go.mod ] || cd flare 2>/dev/null || true
[ -f go.mod ] || { echo "error: run this from the Flare module root (the directory holding go.mod)" >&2; exit 1; }

FAILED=()
SKIPPED=()
# Exit code 99 means "this check did not run". It is distinct from pass and from fail
# on purpose: a skipped security scan that prints ok is worse than no scan at all,
# because it looks like evidence.
step() {
  local name="$1" rc; shift
  printf '\n=== %s ===\n' "$name"
  "$@"; rc=$?
  case "$rc" in
    0)  printf 'ok: %s\n' "$name" ;;
    99) printf 'SKIPPED: %s\n' "$name"; SKIPPED+=("$name") ;;
    *)  printf 'FAIL: %s\n' "$name" >&2; FAILED+=("$name") ;;
  esac
}

# Are we in the published mirror, or in the private monorepo the mirror is cut from?
# The top-level docker-compose.yml is the ESTATE deploy config (it names the house
# proxy network); scripts/split-public-repo.sh strips it from every published commit
# and then asserts it is gone, so its presence is a reliable marker. Public
# self-hosters get deploy/docker-compose.yml, which ships in both places and is
# therefore useless for this. One check below has to mean different things in the two
# places, and getting that wrong makes it either useless upstream or toothless
# downstream.
IN_MIRROR=1
[ -f docker-compose.yml ] && IN_MIRROR=0

# Tools that are not part of the Go toolchain are fetched on demand. Locally that
# needs network; set FLARE_CI_SKIP_TOOLS=1 to skip those checks when offline, and
# say so rather than passing quietly. An install that fails for any other reason is a
# FAILURE, not a skip: "I could not check" must never read as "there is nothing here".
tool() {
  local bin="$1" mod="$2" path
  if command -v "$bin" >/dev/null 2>&1; then command -v "$bin"; return 0; fi
  if [ "${FLARE_CI_SKIP_TOOLS:-0}" = "1" ]; then return 99; fi
  go install "$mod" >/dev/null 2>&1 || { echo "could not install $bin from $mod (offline?)" >&2; return 1; }
  path="$(go env GOPATH)/bin/$bin"
  [ -x "$path" ] || { echo "installed $bin but $path is not executable" >&2; return 1; }
  printf '%s\n' "$path"
}

# gofmt is not a style opinion here, it is the diff-noise gate: one unformatted struct
# literal turns an unrelated review into a whitespace hunt. Checked over TRACKED files
# only, so a vendored or generated tree someone has lying around cannot fail the run.
fmt_check() {
  local unformatted files
  if command -v git >/dev/null 2>&1 && git rev-parse --git-dir >/dev/null 2>&1; then
    files=$(git ls-files '*.go')
  else
    files=$(find . -name '*.go' -not -path './.git/*')
  fi
  [ -n "$files" ] || { echo "no Go files found"; return 1; }
  unformatted=$(printf '%s\n' "$files" | xargs gofmt -l)
  if [ -n "$unformatted" ]; then
    echo "These files are not gofmt-clean:" >&2
    echo "$unformatted" >&2
    echo "Fix with: gofmt -w \$(git ls-files '*.go')" >&2
    return 1
  fi
  echo "every tracked Go file is gofmt-clean"
}

# Goose applies migrations in numeric order and records each applied version in its own
# table, so the FILES and that table have to agree forever. Two things break that
# agreement silently, and both are cheap to catch here:
#
#   duplicate version  goose refuses to run at all ("duplicate version"), which turns a
#                      merge of two branches that both added 026 into a boot failure of
#                      the deployed server, not a CI failure.
#   missing version    a file deleted or renumbered after it was applied somewhere. The
#                      target database still lists that version as applied, so goose has
#                      nothing to compare against and the migration is simply skipped.
#                      An estate deploy already lost an auth migration exactly this way.
#
# The Down half is required because a migration you cannot reverse is a deploy you
# cannot roll back, and that is decided while writing it, not during the incident.
migrations_check() {
  local dir="internal/db/migrations" f base ver want=1 bad=0
  [ -d "$dir" ] || { echo "$dir is missing (internal/db/migrate.go embeds it)" >&2; return 1; }
  for f in "$dir"/*.sql; do
    [ -e "$f" ] || { echo "no migrations found in $dir" >&2; return 1; }
    base="$(basename "$f")"
    if ! [[ "$base" =~ ^[0-9]{3}_[a-z0-9_]+\.sql$ ]]; then
      echo "$base: name must be NNN_lower_snake_case.sql (goose parses the numeric prefix)" >&2
      bad=1; continue
    fi
    ver=$((10#${base:0:3}))
    if [ "$ver" -ne "$want" ]; then
      printf '%s: expected version %03d, got %03d (duplicate or missing version)\n' "$base" "$want" "$ver" >&2
      bad=1
    fi
    want=$((ver + 1))
    grep -qF -- '-- +goose Up' "$f"   || { echo "$base: no '-- +goose Up' annotation" >&2; bad=1; }
    grep -qF -- '-- +goose Down' "$f" || { echo "$base: no '-- +goose Down' annotation, so this deploy cannot be rolled back" >&2; bad=1; }
  done
  [ "$bad" -eq 0 ] || return 1
  echo "$((want - 1)) migrations, contiguous versions, each with a goose Up and Down"
}

# internal/db/generated is sqlc output, and nothing at build time notices when it drifts
# from the queries or the schema it was generated from. The drift is invisible in review
# (the .sql change looks complete) and shows up as a column that reads NULL or a struct
# field that never populates. The version is pinned because sqlc output changes between
# releases: an unpinned sqlc would report a diff that means "newer generator", not
# "stale code". Bump this and regenerate in the same commit.
SQLC_VERSION="v1.30.0"

# gitleaks and govulncheck were installed @latest, so CI executed whatever
# upstream published at that moment, in a job that holds the mirror deploy key.
# A compromised release of either would run with write access to a public repo
# the moment it shipped. Pinned like SQLC_VERSION above; bump deliberately.
GITLEAKS_VERSION="v8.30.0"
GOVULNCHECK_VERSION="v1.1.4"
sqlc_check() {
  local bin rc out
  bin="$(tool sqlc "github.com/sqlc-dev/sqlc/cmd/sqlc@$SQLC_VERSION")"; rc=$?
  [ $rc -eq 0 ] || { [ $rc -eq 99 ] && echo "not run: FLARE_CI_SKIP_TOOLS=1"; return $rc; }
  out="$("$bin" diff 2>&1)"; rc=$?
  if [ $rc -ne 0 ]; then
    printf '%s\n' "$out" >&2
    echo "internal/db/generated is stale. Run: sqlc generate  (and commit the result)" >&2
    return 1
  fi
  echo "internal/db/generated matches internal/db/queries and the migration schema"
}

# The runtime release identity must describe the same artifact as the image a
# Helm install starts. Keep the single chart release input (image.tag) aligned
# with flare/VERSION and Chart.yaml, and keep the deployment template wired to
# pass that value through FLARE_RELEASE. Without this check a healthy pod can
# report a stale release while its image tag points at a different build.
helm_release_identity_check() {
  local release
  [ -f VERSION ] || { echo "VERSION is missing" >&2; return 1; }
  release="$(tr -d '[:space:]' < VERSION)"
  [ -n "$release" ] || { echo "VERSION is empty" >&2; return 1; }
  grep -qF -- "appVersion: \"$release\"" deploy/helm/flare/Chart.yaml || {
    echo "Helm Chart.yaml appVersion does not match VERSION ($release)" >&2
    return 1
  }
  grep -qF -- "  tag: $release" deploy/helm/flare/values.yaml || {
    echo "Helm values image.tag does not match VERSION ($release)" >&2
    return 1
  }
  grep -qF -- 'value: {{ .Values.image.tag | quote }}' deploy/helm/flare/templates/deployment.yaml || {
    echo "Helm deployment does not pass image.tag through FLARE_RELEASE" >&2
    return 1
  }
  [ "$(grep -cF -- "ARG FLARE_RELEASE=$release" Dockerfile)" -eq 2 ] || {
    echo "Dockerfile build stages do not both default to VERSION ($release)" >&2
    return 1
  }
  for compose in docker-compose.yml deploy/docker-compose.yml; do
    [ "$(grep -cF -- "FLARE_RELEASE: \"\${FLARE_RELEASE:-$release}\"" "$compose")" -eq 2 ] || {
      echo "$compose build and runtime release defaults do not both match VERSION ($release)" >&2
      return 1
    }
  done
  echo "Release identity is pinned to $release (Helm, Dockerfile, Compose build and runtime)"
}

# The dashboard is not a separate deliverable: cmd/server does //go:embed all:frontend/build,
# so whatever bun produces is what the single binary serves. A frontend that does not
# typecheck or does not build is a broken release even though every Go gate is green.
FRONTEND="frontend"
bun_step() {
  if ! command -v bun >/dev/null 2>&1; then
    echo "not run: bun is not installed (https://bun.sh). The Docker build installs it for you."
    return 99
  fi
  ( cd "$FRONTEND" && "$@" )
}

# Browser capture is shipped as a small framework-neutral bridge. Keep its
# runtime contract in the same gate as the dashboard so a TypeScript change
# cannot silently break the web SDK while the Svelte app still builds. The
# mirrored first-party bridges are checked when this script runs from the
# monorepo; the public Flare mirror only has examples/browser. First-party
# roots use both `frontend/src/lib` and `src/lib` layouts.
browser_rum_check() {
  if ! command -v bun >/dev/null 2>&1; then
    echo "not run: bun is not installed (https://bun.sh). The Docker build installs it for you."
    return 99
  fi
  ( cd examples/browser && bun test flare-browser.test.ts ) || return $?

  local files=() file tmp
  while IFS= read -r file; do files+=("$file"); done < <(
    find .. \( -path '*/frontend/src/lib/flare-client.ts' -o -path '*/src/lib/flare-client.ts' \) -type f -print 2>/dev/null | sort
  )
  if [ ${#files[@]} -eq 0 ]; then
    echo "no mirrored first-party browser bridges found; example bridge test passed"
    return 0
  fi
  tmp="$(mktemp -d "${TMPDIR:-/tmp}/flare-rum-ci.XXXXXX")"
  # RETURN traps run with nounset still enabled, and the trap otherwise leaks
  # into the caller after this helper returns. Remove it after the first run
  # and tolerate the local name being out of scope on shells that re-fire it.
  trap 'rm -rf -- "${tmp:-}"; trap - RETURN' RETURN
  for file in "${files[@]}"; do
    bun build "$file" --target browser --outfile "$tmp/$(basename "$(dirname "$(dirname "$file")")").js" >/dev/null || {
      echo "browser bridge failed to transpile: $file" >&2
      return 1
    }
  done
  echo "browser example and ${#files[@]} mirrored bridge(s) passed"
}

# A bridge that compiles but is never installed is silent telemetry loss. Keep
# the first-party wiring contract beside the transpile gate: every shipped web
# entrypoint must install both runtime exception capture and RUM, and every
# mirrored bridge must still be present. The public Flare mirror has only the
# example bridge, so this check is intentionally skipped there.
first_party_browser_wiring_check() {
  local repo_root spec bridge entry bridge_count
  # A public Flare checkout has neither sibling app root. If one marker is
  # present, this is the monorepo and a missing sibling must fail loudly
  # rather than turning the whole first-party check into a skip.
  if [ ! -d ../atomicsite ] && [ ! -d ../brightcrm ]; then
    echo "not run: first-party app roots are not present (public Flare mirror)"
    return 99
  fi
  if [ ! -d ../atomicsite ] || [ ! -d ../brightcrm ]; then
    echo "first-party app roots are incomplete; refusing to skip browser wiring" >&2
    return 1
  fi

  local -a specs=(
    "atomicsite/frontend/src/lib/flare-client.ts|atomicsite/frontend/src/routes/+layout.svelte"
    "bright-interaction-website/src/lib/flare-client.ts|bright-interaction-website/src/layouts/Layout.astro"
    "brightcommunity/frontend/src/lib/flare-client.ts|brightcommunity/frontend/src/routes/+layout.svelte"
    "brightcrm/frontend/src/lib/flare-client.ts|brightcrm/frontend/src/routes/+layout.svelte"
    "dockyard/frontend/src/lib/flare-client.ts|dockyard/frontend/src/main.tsx"
    "hash/frontend/src/lib/flare-client.ts|hash/frontend/src/routes/+layout.svelte"
    "portal/src/lib/flare-client.ts|portal/src/main.tsx"
    "sentinel/frontend/src/lib/flare-client.ts|sentinel/frontend/src/main.tsx"
    "svar/portal/frontend/src/lib/flare-client.ts|svar/portal/frontend/src/main.tsx"
    "trustissues/frontend/src/lib/flare-client.ts|trustissues/frontend/src/main.tsx"
    "flare/frontend/src/lib/flare-client.ts|flare/frontend/src/routes/+layout.svelte"
  )
  repo_root=".."
  bridge_count="$(find "$repo_root" \( -path '*/frontend/src/lib/flare-client.ts' -o -path '*/src/lib/flare-client.ts' \) -type f -print 2>/dev/null | wc -l | tr -d ' ')"
  if [ "$bridge_count" -ne "${#specs[@]}" ]; then
    echo "first-party bridge inventory changed: found $bridge_count, expected ${#specs[@]} (update this gate and the integration map)" >&2
    return 1
  fi
  for spec in "${specs[@]}"; do
    bridge="${spec%%|*}"
    entry="${spec#*|}"
    if [ ! -f "$repo_root/$bridge" ]; then
      echo "missing first-party browser bridge: $bridge" >&2
      return 1
    fi
    if [ ! -f "$repo_root/$entry" ]; then
      echo "missing first-party browser entrypoint: $entry" >&2
      return 1
    fi
    if ! grep -Eq 'export[[:space:]]+function[[:space:]]+installFlareBrowserCapture' "$repo_root/$bridge" ||
      ! grep -Eq 'export[[:space:]]+function[[:space:]]+installFlareBrowserRum' "$repo_root/$bridge"; then
      echo "first-party bridge does not export capture and RUM: $bridge" >&2
      return 1
    fi
    if ! grep -Eq 'installFlareBrowserCapture[[:space:]]*\(' "$repo_root/$entry" ||
      ! grep -Eq 'installFlareBrowserRum[[:space:]]*\(' "$repo_root/$entry"; then
      echo "first-party entrypoint does not install capture and RUM: $entry" >&2
      return 1
    fi
  done

  echo "${#specs[@]} first-party browser bridges are present and installed"
}

# The scanners below read CONTENT, never a pipeline's exit status. `producer | grep -q X`
# under pipefail returns 141 when grep short-circuits and the producer takes SIGPIPE, so
# that shape reports "clean" exactly when there is the most to find.
secret_scan() {
  local bin rc args
  bin="$(tool gitleaks "github.com/zricethezav/gitleaks/v8@$GITLEAKS_VERSION")"; rc=$?
  [ $rc -eq 0 ] || { [ $rc -eq 99 ] && echo "not run: FLARE_CI_SKIP_TOOLS=1"; return $rc; }
  # In the mirror every published commit is this project's, so scanning the full history
  # is exactly right and is the last gate before a credential becomes world-readable.
  # Upstream the history belongs to a whole monorepo of unrelated projects, so a full
  # scan reports hundreds of findings that have nothing to do with Flare and would train
  # everyone to ignore the check. There the working tree is what matters, and
  # scripts/split-public-repo.sh scans the real filtered payload's history before it pushes.
  if [ "$IN_MIRROR" = "1" ]; then
    args=(detect --source . --config .gitleaks.toml --no-banner --redact)
  else
    args=(detect --source . --config .gitleaks.toml --no-git --no-banner --redact)
  fi
  "$bin" "${args[@]}"
}

# Advisories Flare is allowed to carry. ONLY for a finding with no upstream fix whose
# exploit precondition is documented absent. Anything with a released fix gets the
# dependency bumped instead, because an entry here is permanent by default.
#
#   GO-2025-3884  gorilla/csrf v1.7.3, "Fixed in: N/A". The TrustedOrigins bypass needs a
#                 plaintext-http trusted origin; Flare pins a static https BASE_URL as its
#                 only trusted origin and sets SameSite=Strict session cookies. Drop this
#                 entry the day upstream ships a release.
#
# This mirrors the estate ci-go gate's accepted list for flare, deliberately: two gates
# disagreeing about what is acceptable means one of them gets ignored.
VULN_ACCEPTED=(GO-2025-3884)
vuln_scan() {
  local bin rc out ids id unexpected=() accepted_seen=()
  bin="$(tool govulncheck "golang.org/x/vuln/cmd/govulncheck@$GOVULNCHECK_VERSION")"; rc=$?
  [ $rc -eq 0 ] || { [ $rc -eq 99 ] && echo "not run: FLARE_CI_SKIP_TOOLS=1"; return $rc; }
  out="$("$bin" ./... 2>&1)"; rc=$?
  if [ $rc -eq 0 ]; then
    echo "no vulnerability is reachable from Flare's code"
    [ ${#VULN_ACCEPTED[@]} -gt 0 ] && echo "note: VULN_ACCEPTED still lists ${VULN_ACCEPTED[*]}, which no longer appears. Remove it."
    return 0
  fi
  # Only the ids under "=== Symbol Results ===" are reachable from Flare's own code.
  # govulncheck prints them as "Vulnerability #N: GO-...."; anything it merely found in
  # an imported package is summarised as a count, without an id, and is not a gate.
  ids=$(printf '%s\n' "$out" | grep -oE '^Vulnerability #[0-9]+: GO-[0-9]{4}-[0-9]+' | grep -oE 'GO-[0-9]{4}-[0-9]+' | sort -u)
  if [ -z "$ids" ]; then
    printf '%s\n' "$out" >&2
    echo "govulncheck exited $rc without naming an advisory, so this is a tool or toolchain failure, not a clean scan." >&2
    return 1
  fi
  for id in $ids; do
    if printf '%s\n' "${VULN_ACCEPTED[@]}" | grep -qx "$id"; then
      accepted_seen+=("$id")
    else
      unexpected+=("$id")
    fi
  done
  if [ ${#unexpected[@]} -ne 0 ]; then
    printf '%s\n' "$out" >&2
    echo "Reachable advisories that are NOT on the accepted list: ${unexpected[*]}" >&2
    echo "Bump the dependency. Only add to VULN_ACCEPTED when upstream has no fix at all." >&2
    return 1
  fi
  echo "only accepted advisories are reachable: ${accepted_seen[*]}"
}

step "build"                                  go build ./...
step "vet"                                    go vet ./...
step "test (RUN, not just compile)"           go test ./... -count=1 -timeout=600s
step "gofmt"                                  fmt_check
step "goose migrations"                       migrations_check
step "sqlc generated code is current"         sqlc_check
step "Helm release identity"                  helm_release_identity_check
step "frontend deps (frozen lockfile)"        bun_step bun install --frozen-lockfile
step "frontend typecheck (svelte-check)"      bun_step bun run check
step "frontend build (what the binary embeds)" bun_step bun run build
step "browser RUM bridge tests and transpile" browser_rum_check
step "first-party browser bridge wiring" first_party_browser_wiring_check
if [ "$IN_MIRROR" = "1" ]; then
  step "secret scan (full history)" secret_scan
else
  step "secret scan (working tree; split-public-repo.sh scans the filtered history)" secret_scan
fi
step "vulnerability scan (govulncheck)"       vuln_scan

printf '\n'
if [ ${#SKIPPED[@]} -ne 0 ]; then
  printf "ci: %d check(s) DID NOT RUN: %s\n" "${#SKIPPED[@]}" "${SKIPPED[*]}" >&2
fi
if [ ${#FAILED[@]} -ne 0 ]; then
  # Report EVERY failure, not just the first. A run that stops at the first problem
  # costs a full round trip per issue.
  printf 'ci: %d check(s) failed: %s\n' "${#FAILED[@]}" "${FAILED[*]}" >&2
  exit 1
fi
if [ ${#SKIPPED[@]} -ne 0 ]; then
  echo "ci: every check that RAN passed, but some did not run (see above)"
  exit 0
fi
echo "ci: all checks passed"
