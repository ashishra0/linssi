#!/bin/sh
# linssi installer: curl -fsSL https://linssi.app/install | sh
set -eu
REPO="ashishra0/linssi"
DIR="${LINSSI_INSTALL_DIR:-$HOME/.local/bin}"
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64)  arch=amd64 ;;
  *) echo "error: no prebuilt binary for $(uname -m); run: go install github.com/$REPO@latest" >&2; exit 1 ;;
esac
case "$os" in darwin|linux) ;; *) echo "error: unsupported OS $os" >&2; exit 1 ;; esac
echo "downloading linssi ($os/$arch)…"
mkdir -p "$DIR"
curl -fsSL "https://github.com/$REPO/releases/latest/download/linssi-$os-$arch.tar.gz" | tar -xz -C "$DIR" linssi
chmod +x "$DIR/linssi"
echo "installed $("$DIR/linssi" --version) → $DIR/linssi"
case ":$PATH:" in
  *":$DIR:"*) echo "next: claude mcp add linssi -- linssi mcp" ;;
  *) echo "note: $DIR is not on your PATH. Add it, or give agents the full path:"
     echo "      claude mcp add linssi -- $DIR/linssi mcp" ;;
esac
