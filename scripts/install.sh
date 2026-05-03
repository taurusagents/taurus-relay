#!/bin/sh

set -u

TAURUS_RELAY_REPO=${TAURUS_RELAY_REPO:-taurusagents/taurus-relay}
TAURUS_URL=${TAURUS_URL:-https://app.taurus.cloud}
TAURUS_RELAY_VERSION=${TAURUS_RELAY_VERSION:-latest}
TAURUS_RELAY_SKIP_CONNECT=${TAURUS_RELAY_SKIP_CONNECT:-0}
TAURUS_INSTALL_DIR=${TAURUS_INSTALL_DIR:-}

log() {
  printf '%s\n' "$*"
}

fail() {
  printf 'Error: %s\n' "$*" >&2
  exit 1
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

normalize_url() {
  printf '%s' "$1" | sed 's#/*$##'
}

resolve_install_dir() {
  if [ -n "$TAURUS_INSTALL_DIR" ]; then
    printf '%s\n' "$TAURUS_INSTALL_DIR"
    return
  fi

  if [ "$(id -u)" -eq 0 ]; then
    printf '%s\n' "/usr/local/bin"
    return
  fi

  [ -n "${HOME:-}" ] || fail 'HOME is not set; set TAURUS_INSTALL_DIR explicitly'
  printf '%s\n' "$HOME/.local/bin"
}

resolve_platform() {
  os=$(uname -s 2>/dev/null | tr '[:upper:]' '[:lower:]')
  arch=$(uname -m 2>/dev/null)

  case "$os" in
    linux) os='linux' ;;
    darwin) os='darwin' ;;
    *) fail "unsupported OS: $os (expected linux or darwin)" ;;
  esac

  case "$arch" in
    x86_64|amd64) arch='amd64' ;;
    arm64|aarch64) arch='arm64' ;;
    *) fail "unsupported architecture: $arch (expected amd64 or arm64)" ;;
  esac

  printf '%s %s\n' "$os" "$arch"
}

verify_checksum() {
  archive_path=$1
  checksums_path=$2
  archive_name=$3

  expected=$(awk -v name="$archive_name" '$2 == name { print $1; exit }' "$checksums_path")
  [ -n "$expected" ] || fail "could not find checksum for ${archive_name}"

  if command -v sha256sum >/dev/null 2>&1; then
    printf '%s  %s\n' "$expected" "$archive_path" | sha256sum -c - >/dev/null || fail "checksum verification failed for ${archive_name}"
    return
  fi

  if command -v shasum >/dev/null 2>&1; then
    actual=$(shasum -a 256 "$archive_path" | awk '{print $1}')
    [ "$actual" = "$expected" ] || fail "checksum verification failed for ${archive_name}"
    return
  fi

  log 'Warning: neither sha256sum nor shasum is available; skipping checksum verification.'
}

need_cmd curl
need_cmd tar
install_dir=$(resolve_install_dir)
mkdir -p "$install_dir" || fail "failed to create install dir: $install_dir"
[ -w "$install_dir" ] || fail "install dir is not writable: $install_dir"

set -- $(resolve_platform)
os=$1
arch=$2

version=$TAURUS_RELAY_VERSION
archive_name="taurus-relay_${os}_${arch}.tar.gz"
if [ "$version" = 'latest' ]; then
  release_base_url="https://github.com/${TAURUS_RELAY_REPO}/releases/latest/download"
  release_label='latest release'
elif [ "${version#v}" = "$version" ]; then
  version="v${version}"
  release_base_url="https://github.com/${TAURUS_RELAY_REPO}/releases/download/${version}"
  release_label=$version
else
  release_base_url="https://github.com/${TAURUS_RELAY_REPO}/releases/download/${version}"
  release_label=$version
fi
archive_url="${release_base_url}/${archive_name}"
checksums_url="${release_base_url}/checksums.txt"

tmpdir=$(mktemp -d 2>/dev/null || mktemp -d -t taurus-relay-installer)
trap 'rm -rf "$tmpdir"' 0 HUP INT TERM

archive_path="$tmpdir/$archive_name"
checksums_path="$tmpdir/checksums.txt"
extract_dir="$tmpdir/extract"
mkdir -p "$extract_dir"

log "Downloading ${archive_name} (${release_label})..."
curl -fsSL "$archive_url" -o "$archive_path" || fail "failed to download ${archive_url}"
curl -fsSL "$checksums_url" -o "$checksums_path" || fail "failed to download checksums.txt"
verify_checksum "$archive_path" "$checksums_path" "$archive_name"

tar -xzf "$archive_path" -C "$extract_dir" || fail "failed to extract ${archive_name}"
[ -f "$extract_dir/taurus-relay" ] || fail 'archive did not contain taurus-relay binary'

binary_path="$install_dir/taurus-relay"
cp "$extract_dir/taurus-relay" "$binary_path" || fail "failed to copy binary to ${binary_path}"
chmod 0755 "$binary_path" || fail "failed to chmod ${binary_path}"

log "Installed taurus-relay to ${binary_path}"
case ":$PATH:" in
  *":$install_dir:"*) ;;
  *) log "Note: ${install_dir} is not currently on PATH." ;;
esac

if [ "$TAURUS_RELAY_SKIP_CONNECT" = '1' ]; then
  log 'Skipping taurus-relay connect because TAURUS_RELAY_SKIP_CONNECT=1.'
  exit 0
fi

[ -n "${TAURUS_TOKEN:-}" ] || fail 'TAURUS_TOKEN is required unless TAURUS_RELAY_SKIP_CONNECT=1'
TAURUS_URL=$(normalize_url "$TAURUS_URL")

set -- connect --token "$TAURUS_TOKEN" --server "$TAURUS_URL"
case "$TAURUS_URL" in
  http://*)
    log "Warning: ${TAURUS_URL} is non-TLS; passing --insecure to taurus-relay connect."
    set -- connect --insecure --token "$TAURUS_TOKEN" --server "$TAURUS_URL"
    ;;
esac

log "Starting taurus-relay connect against ${TAURUS_URL}..."
exec "$binary_path" "$@"
