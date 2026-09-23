#!/bin/sh
# Install the latest Agentium release: curl -fsSL <raw url>/install.sh | sh
# Env: AGENTIUM_VERSION (tag, default latest), AGENTIUM_BIN_DIR (default ~/.local/bin)
set -eu
repo="TegarTheGreat/Agentium"
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
case "$os" in linux|darwin) ;; *) echo "unsupported OS: $os (use go install)" >&2; exit 1 ;; esac
if [ -n "${AGENTIUM_DOWNLOAD_BASE:-}" ]; then
  base="$AGENTIUM_DOWNLOAD_BASE"
elif [ -n "${AGENTIUM_VERSION:-}" ]; then
  base="https://github.com/$repo/releases/download/$AGENTIUM_VERSION"
else
  base="https://github.com/$repo/releases/latest/download"
fi
file="agentium_${os}_${arch}.tar.gz"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
dir="${AGENTIUM_BIN_DIR:-$HOME/.local/bin}"
echo "downloading $base/$file"
if ! curl -fsSL "$base/$file" -o "$tmp/$file"; then
  # No published release (yet): build from source when Go is available.
  if command -v go >/dev/null 2>&1; then
    echo "no release binary found; building from source with go install" >&2
    mkdir -p "$dir"
    GOBIN="$dir" CGO_ENABLED=0 go install -trimpath -ldflags "-s -w" \
      "github.com/tegarthegreat/agentium/cmd/agentium@${AGENTIUM_VERSION:-latest}"
    echo "installed $("$dir/agentium" version) to $dir/agentium"
    case ":$PATH:" in *":$dir:"*) ;; *) echo "add $dir to your PATH" ;; esac
    exit 0
  fi
  echo "no release binary at $base/$file; install Go 1.24+ and run:" >&2
  echo "  go install github.com/tegarthegreat/agentium/cmd/agentium@latest" >&2
  exit 1
fi
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"
want=$(grep " $file\$" "$tmp/checksums.txt" | cut -d' ' -f1)
if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum "$tmp/$file" | cut -d' ' -f1)
else got=$(shasum -a 256 "$tmp/$file" | cut -d' ' -f1); fi
[ -n "$want" ] && [ "$want" = "$got" ] || { echo "checksum mismatch for $file" >&2; exit 1; }
tar -xzf "$tmp/$file" -C "$tmp"
mkdir -p "$dir"
install -m 0755 "$tmp/agentium" "$dir/agentium"
echo "installed $("$dir/agentium" version) to $dir/agentium"
case ":$PATH:" in *":$dir:"*) ;; *) echo "add $dir to your PATH" ;; esac
