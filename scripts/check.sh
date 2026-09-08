#!/usr/bin/env bash
# Everything CI checks, runnable locally in a few seconds. CI calls this script
# too, so the two cannot drift apart — a local check that verifies something
# different from CI stops being worth running.
#
#   ./scripts/check.sh          gofmt, vet, tests, and all five release targets
#   ./scripts/check.sh --quick  skip the cross-compiles
#
# Run it before pushing. There is deliberately no git hook: on this machine
# core.hooksPath is claimed at system level by git-defender, and pointing it at a
# repo-local directory silently disables that — secret scanning included. See
# "CI" in README.md.
set -euo pipefail

cd "$(dirname "$0")/.."

QUICK=false
[ "${1:-}" = "--quick" ] && QUICK=true

fail() { printf '\n\033[31m✗ %s\033[0m\n' "$1" >&2; exit 1; }
step() { printf '\033[2m→ %s\033[0m\n' "$1"; }

step "gofmt"
unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
  echo "$unformatted" >&2
  gofmt -d . >&2
  fail "not gofmt'd (run: gofmt -w .)"
fi

step "go vet"
go vet ./... || fail "go vet found problems"

# -count=1 defeats the test cache, so a pass means the tests actually ran.
step "go test"
go test ./... -count=1 || fail "tests failed"

# The version constant is what install.sh compares against the release tag.
step "version constant"
version="$(awk -F'"' '/^const version = /{print $2; exit}' cmd/asbutler/main.go)"
case "$version" in
  [0-9]*.[0-9]*.[0-9]*) ;;
  *) fail "version \"$version\" in cmd/asbutler/main.go is not X.Y.Z" ;;
esac

if ! $QUICK; then
  # Compiling every target is what catches a build-tagged file (lock_unix.go,
  # lock_windows.go) changed on one platform and broken on the other.
  step "cross-compile (5 targets)"
  for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do
    os="${target%/*}"; arch="${target#*/}"
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -o /dev/null ./cmd/asbutler \
      || fail "build failed for $target"
  done
fi

printf '\n\033[32m✓ all checks passed\033[0m (v%s)\n' "$version"
