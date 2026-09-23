#!/bin/sh
# Install Agentium: curl -fsSL https://raw.githubusercontent.com/TegarTheGreat/Agentium/main/install.sh | sh
# Env: AGENTIUM_VERSION (tag, default latest), AGENTIUM_BIN_DIR (default ~/.local/bin)
set -eu
repo="TegarTheGreat/Agentium"
dir="${AGENTIUM_BIN_DIR:-$HOME/.local/bin}"

# Output: colors and a progress bar only on a terminal.
if [ -t 2 ] && [ -z "${NO_COLOR:-}" ]; then
  bold=$(printf '\033[1m'); dim=$(printf '\033[2m'); green=$(printf '\033[32m')
  red=$(printf '\033[31m'); cyan=$(printf '\033[36m'); reset=$(printf '\033[0m')
  progress="--progress-bar"
else
  bold=; dim=; green=; red=; cyan=; reset=; progress="-sS"
fi
step() { printf '%s==>%s %s%s%s\n' "$cyan" "$reset" "$bold" "$*" "$reset" >&2; }
info() { printf '    %s%s%s\n' "$dim" "$*" "$reset" >&2; }
ok()   { printf '%s ✓%s  %s\n' "$green" "$reset" "$*" >&2; }
fail() { printf '%s ✗%s  %s\n' "$red" "$reset" "$*" >&2; exit 1; }

# spin CMD... runs a command with a spinner (on a terminal) and its output hidden.
spin() {
  log="$tmp/spin.log"
  "$@" >"$log" 2>&1 &
  pid=$!
  if [ -t 2 ]; then
    i=0
    while kill -0 "$pid" 2>/dev/null; do
      case $((i % 4)) in 0) c='|' ;; 1) c='/' ;; 2) c='-' ;; *) c='\' ;; esac
      printf '\r    %s%s working…%s' "$dim" "$c" "$reset" >&2
      i=$((i + 1))
      sleep 1
    done
    printf '\r\033[K' >&2
  fi
  if ! wait "$pid"; then
    cat "$log" >&2
    return 1
  fi
}

finish() {
  ok "Installed $("$dir/agentium" version) to $dir/agentium"
  case ":$PATH:" in
    *":$dir:"*) ;;
    *) printf '\n    Add it to your PATH:\n      %sexport PATH="%s:$PATH"%s\n' "$bold" "$dir" "$reset" >&2 ;;
  esac
  printf '\n    Get started:\n      %sagentium login anthropic%s   %s# or openai, gemini, openrouter, ...%s\n      %sagentium%s\n\n' \
    "$bold" "$reset" "$dim" "$reset" "$bold" "$reset" >&2
}

printf '\n%sAgentium installer%s\n\n' "$bold" "$reset" >&2

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) fail "Unsupported architecture: $(uname -m)" ;;
esac
case "$os" in linux|darwin) ;; *) fail "Unsupported OS: $os (use go install)" ;; esac
ok "Platform: $os/$arch"

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

step "Downloading $file"
info "$base/$file"
if ! curl -fsSL -r 0-0 -o /dev/null "$base/$file" 2>"$tmp/curl.err"; then
  # No published release: build from source when Go is available.
  if command -v go >/dev/null 2>&1; then
    info "No release binary found; building from source instead"
    step "Building with $(go version | cut -d' ' -f3) (this can take a minute)"
    mkdir -p "$dir"
    spin env GOBIN="$dir" CGO_ENABLED=0 go install -trimpath -ldflags "-s -w" \
      "github.com/tegarthegreat/agentium/cmd/agentium@${AGENTIUM_VERSION:-latest}" ||
      fail "Build failed"
    finish
    exit 0
  fi
  cat "$tmp/curl.err" >&2
  fail "No release binary at $base/$file
    Install Go 1.24+ and run: go install github.com/tegarthegreat/agentium/cmd/agentium@latest"
fi
curl -fL $progress "$base/$file" -o "$tmp/$file" || fail "Download failed"

step "Verifying checksum"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt" || fail "Could not download checksums.txt"
want=$(grep " $file\$" "$tmp/checksums.txt" | cut -d' ' -f1)
if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum "$tmp/$file" | cut -d' ' -f1)
else got=$(shasum -a 256 "$tmp/$file" | cut -d' ' -f1); fi
[ -n "$want" ] && [ "$want" = "$got" ] || fail "Checksum mismatch for $file"
ok "SHA-256 matches"

step "Installing"
tar -xzf "$tmp/$file" -C "$tmp"
mkdir -p "$dir"
install -m 0755 "$tmp/agentium" "$dir/agentium"
finish
