#!/usr/bin/env bash
# Build Agent Session Butler and install/upgrade it to ~/.local/bin.
# Run this the first time and any time you want to upgrade — it's the same script.
set -euo pipefail

cd "$(dirname "$0")"

BIN_DIR="${BIN_DIR:-$HOME/.local/bin}"
BIN="$BIN_DIR/asbutler"

command -v go >/dev/null || { echo "error: Go is not installed or not in PATH" >&2; exit 1; }

# Require the Go version go.mod asks for (read it, don't hardcode). Normalize
# both to X.Y.Z so "1.25" and "1.25.0" compare equal, then check have >= need.
norm() { awk -F. '{printf "%d.%d.%d", $1, ($2==""?0:$2), ($3==""?0:$3)}'; }
need="$(awk '/^go /{print $2; exit}' go.mod)"
have="$(go env GOVERSION | sed 's/^go//')"
if [ -n "$need" ]; then
  lowest="$(printf '%s\n%s\n' "$(echo "$need" | norm)" "$(echo "$have" | norm)" | sort -V | head -1)"
  if [ "$lowest" != "$(echo "$need" | norm)" ]; then
    echo "error: Go $need+ required, but 'go' is $have. Please upgrade Go." >&2
    exit 1
  fi
fi

echo "Building asbutler…"
mkdir -p "$BIN_DIR"
go build -o "$BIN" ./cmd/asbutler

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
