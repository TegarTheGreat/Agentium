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
else
  bold=; dim=; green=; red=; cyan=; reset=
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
      printf '\r    %s%s working… %ss%s' "$dim" "$c" "$((i / 5))" "$reset" >&2
      i=$((i + 1))
      sleep 0.2 2>/dev/null || { sleep 1; i=$((i + 4)); }
    done
    printf '\r\033[K' >&2
  fi
  if ! wait "$pid"; then
    cat "$log" >&2
    return 1
  fi
}

# Retries, and gives up on a stalled connection instead of hanging.
fetch() { curl -fsSL --connect-timeout 15 --retry 3 --speed-limit 1024 --speed-time 30 "$@"; }

mb() { awk -v b="$1" 'BEGIN { printf "%.1f MB", b / 1048576 }'; }

# download URL FILE TOTAL shows a progress bar with percent, size and speed.
download() {
  fetch "$1" -o "$2" 2>"$tmp/curl.err" &
  pid=$!
  start=$(date +%s)
  while kill -0 "$pid" 2>/dev/null; do
    if [ -t 2 ]; then
      got=0; [ -f "$2" ] && got=$(($(wc -c <"$2")))
      secs=$(($(date +%s) - start)); [ "$secs" -gt 0 ] || secs=1
      speed="$(mb $((got / secs)))/s"
      if [ "${3:-0}" -gt 0 ]; then
        pct=$((got * 100 / $3)); fill=$((pct * 30 / 100))
        bar=$(awk -v f="$fill" 'BEGIN { for (i = 0; i < 30; i++) printf (i < f ? "█" : "░") }')
        printf '\r    %s%s%s %3d%%  %s / %s  %s  \033[K' "$cyan" "$bar" "$reset" "$pct" \
          "$(mb "$got")" "$(mb "$3")" "$speed" >&2
      else
        printf '\r    %s  %s  \033[K' "$(mb "$got")" "$speed" >&2
      fi
    fi
    sleep 0.2 2>/dev/null || sleep 1
  done
  [ -t 2 ] && printf '\r\033[K' >&2
  if ! wait "$pid"; then
    cat "$tmp/curl.err" >&2
    return 1
  fi
}

finish() {
  ok "Installed $("$dir/agentium" version) to $dir/agentium"
  case ":$PATH:" in
    *":$dir:"*) ;;
    *) printf '\n    Add it to your PATH:\n      %sexport PATH="%s:$PATH"%s\n' "$bold" "$dir" "$reset" >&2 ;;
  esac
  printf '\n    Get started:\n      %sagentium%s   %s# the first run asks for a provider and API key%s\n\n' \
    "$bold" "$reset" "$dim" "$reset" >&2
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
# A one-byte range request checks the release exists and reveals its size.
if fetch -r 0-0 -D "$tmp/headers" -o /dev/null "$base/$file" 2>"$tmp/curl.err"; then
  total=$(tr -d '\r' <"$tmp/headers" | awk -F/ 'tolower($0) ~ /^content-range:/ { n = $NF } END { print n + 0 }')
else
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
download "$base/$file" "$tmp/$file" "$total" || fail "Download failed (check your connection and try again)"
ok "Downloaded $(mb "$(wc -c <"$tmp/$file")")"

step "Verifying checksum"
fetch "$base/checksums.txt" -o "$tmp/checksums.txt" || fail "Could not download checksums.txt"
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
