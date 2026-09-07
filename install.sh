#!/usr/bin/env bash
# Install/upgrade Agent Session Butler to ~/.local/bin. Rerun any time to upgrade.
#   ./install.sh          download the matching prebuilt binary (default, fast)
#   ./install.sh --build  build from source instead (needs Go 1.25+)
set -euo pipefail

cd "$(dirname "$0")"

REPO="aleck31/agent-session-butler"
BIN_DIR="${BIN_DIR:-$HOME/.local/bin}"
BIN="$BIN_DIR/asbutler"
FROM_SOURCE=false
[ "${1:-}" = "--build" ] && FROM_SOURCE=true

norm() { awk -F. '{printf "%d.%d.%d", $1, ($2==""?0:$2), ($3==""?0:$3)}'; }

# True if `go` is present and new enough for go.mod's directive.
go_ok() {
  command -v go >/dev/null || return 1
  [ -f go.mod ] || return 0
  local need have lowest
  need="$(awk '/^go /{print $2; exit}' go.mod)"
  have="$(go env GOVERSION | sed 's/^go//')"
  [ -n "$need" ] || return 0
  lowest="$(printf '%s\n%s\n' "$(echo "$need" | norm)" "$(echo "$have" | norm)" | sort -V | head -1)"
  [ "$lowest" = "$(echo "$need" | norm)" ]
}

# The version the checked-out source would build, e.g. 0.7.0.
source_version() {
  awk -F'"' '/^const version = /{print $2; exit}' cmd/asbutler/main.go 2>/dev/null
}

# True if $1 is strictly newer than $2 (both bare X.Y.Z).
newer_than() {
  [ -n "$1" ] && [ -n "$2" ] || return 1
  [ "$(echo "$1" | norm)" != "$(echo "$2" | norm)" ] &&
    [ "$(printf '%s\n%s\n' "$(echo "$1" | norm)" "$(echo "$2" | norm)" | sort -V | tail -1)" = "$(echo "$1" | norm)" ]
}

build_from_source() {
  echo "Building asbutler from source…"
  mkdir -p "$BIN_DIR"
  go build -o "$BIN" ./cmd/asbutler
}

# Map this machine to the release asset suffix, e.g. linux-amd64 / darwin-arm64.
asset_suffix() {
  local os arch ext=""
  case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    MINGW*|MSYS*|CYGWIN*) os=windows; ext=.exe ;;
    *) echo "unsupported OS: $(uname -s)" >&2; return 1 ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) echo "unsupported arch: $(uname -m)" >&2; return 1 ;;
  esac
  echo "${os}-${arch}${ext}"
}

# gh first: it carries the user's credentials, the only way to reach release
# assets while the repo is private (anonymous URLs 404 there).
gh_ready() { command -v gh >/dev/null && gh auth status >/dev/null 2>&1; }

# Latest release tag (e.g. v0.7.0), or empty. Cached so it is resolved once.
LATEST_TAG=""
latest_release_tag() {
  [ -n "$LATEST_TAG" ] && { echo "$LATEST_TAG"; return 0; }
  if gh_ready; then
    LATEST_TAG="$(gh release view --repo "$REPO" --json tagName --jq .tagName 2>/dev/null || true)"
  fi
  if [ -z "$LATEST_TAG" ] && command -v curl >/dev/null; then
    # No awk 'exit' — closing the pipe early makes curl fail under pipefail;
    # read to EOF and keep the first match.
    LATEST_TAG="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null \
      | awk -F'"' '/"tag_name"/ && !seen {print $4; seen=1}' || true)"
  fi
  echo "$LATEST_TAG"
}

download_via_gh() {
  local suffix="$1" tag="$2"
  gh_ready || return 1
  echo "Downloading prebuilt binary via gh ($tag, $suffix)…"
  mkdir -p "$BIN_DIR"
  gh release download "$tag" --repo "$REPO" \
    --pattern "asbutler-$tag-$suffix" --output "$BIN" --clobber
}

# Anonymous download — works once the repo is public.
download_via_curl() {
  local suffix="$1" tag="$2" url
  command -v curl >/dev/null || return 1
  url="https://github.com/$REPO/releases/download/$tag/asbutler-$tag-$suffix"
  echo "Downloading prebuilt binary ($tag, $suffix)…"
  echo "  $url"
  mkdir -p "$BIN_DIR"
  curl -fSL "$url" -o "$BIN" || return 1
}

download_release() {
  local suffix tag
  suffix="$(asset_suffix)" || exit 1
  tag="$(latest_release_tag)"
  [ -n "$tag" ] || { echo "error: could not resolve the latest release tag" >&2; exit 1; }

  if download_via_gh "$suffix" "$tag" || download_via_curl "$suffix" "$tag"; then
    chmod +x "$BIN"
    return 0
  fi

  echo "error: could not download a prebuilt binary for $suffix." >&2
  echo "  If the release assets need credentials, install the GitHub CLI and run" >&2
  echo "  'gh auth login'. Otherwise build from source:" >&2
  echo "    ./install.sh --build" >&2
  exit 1
}

# Default to the prebuilt binary (fast, no toolchain). --build compiles the
# checked-out source — that's the path to use when you've changed the code.
#
# But never silently downgrade: running this from a checkout that is ahead of the
# latest release used to install the older release over the newer source, which
# looks like "upgrade did nothing". When the source is ahead, build it.
if ! $FROM_SOURCE; then
  src="$(source_version)"
  rel="$(latest_release_tag | sed 's/^v//')"
  if newer_than "$src" "$rel"; then
    if go_ok; then
      echo "note: this checkout is v$src, ahead of the latest release v$rel — building from source."
      FROM_SOURCE=true
    else
      echo "warning: this checkout is v$src but the latest release is v$rel, and Go is not available" >&2
      echo "         to build it — installing the older release v$rel instead." >&2
    fi
  fi
fi

if $FROM_SOURCE; then
  go_ok || { echo "error: --build needs Go $(awk '/^go /{print $2; exit}' go.mod)+ installed" >&2; exit 1; }
  build_from_source
else
  download_release
fi

echo "Installed: $BIN ($("$BIN" version))"

# Warn if the install dir isn't on PATH, with the line to fix it.
case ":$PATH:" in
  *":$BIN_DIR:"*) echo "Run it with:  asbutler webui" ;;
  *)
    echo
    echo "note: $BIN_DIR is not on your PATH. Add it, e.g.:"
    echo "  echo 'export PATH=\"$BIN_DIR:\$PATH\"' >> ~/.bashrc && source ~/.bashrc"
    ;;
esac
