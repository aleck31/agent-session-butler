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

download_release() {
  local suffix tag url
  suffix="$(asset_suffix)" || exit 1

  command -v curl >/dev/null || { echo "error: curl is required to download (or use --build)" >&2; exit 1; }

  echo "Downloading the latest prebuilt binary ($suffix)…"
  # Latest release tag via the GitHub API. No awk 'exit' — closing the pipe
  # early makes curl fail under pipefail; read to EOF and keep the first match.
  tag="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
    | awk -F'"' '/"tag_name"/ && !seen {print $4; seen=1}')"
  [ -n "$tag" ] || { echo "error: could not resolve latest release tag" >&2; exit 1; }

  url="https://github.com/$REPO/releases/download/$tag/asbutler-$tag-$suffix"
  mkdir -p "$BIN_DIR"
  echo "  $url"
  curl -fSL "$url" -o "$BIN"
  chmod +x "$BIN"
}

# Default to the prebuilt binary (fast, no toolchain). --build compiles the
# checked-out source — that's the path to use when you've changed the code.
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
