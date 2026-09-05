#!/bin/sh

set -u

TAURUS_RELAY_REPO=${TAURUS_RELAY_REPO:-taurusagents/taurus-relay}
TAURUS_URL=${TAURUS_URL:-https://app.taurusagents.com}
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

# The two resolvers below are called inside a command substitution, so they
# must not call fail(): the exit would only end the subshell and the script
# would keep running with an empty value. Instead they print the error message
# on stdout and return non-zero. Because the substitution captures stdout, the
# caller's variable holds the message, and every call site is written as
#   value=$(resolve_...) || fail "$value"
resolve_install_dir() {
  if [ -n "$TAURUS_INSTALL_DIR" ]; then
    printf '%s\n' "$TAURUS_INSTALL_DIR"
    return 0
  fi

  if [ "$(id -u)" -eq 0 ]; then
    printf '%s\n' "/usr/local/bin"
    return 0
  fi

  if [ -z "${HOME:-}" ]; then
    printf '%s\n' 'HOME is not set; set TAURUS_INSTALL_DIR explicitly'
    return 1
  fi
  printf '%s\n' "$HOME/.local/bin"
}

resolve_platform() {
  os=$(uname -s 2>/dev/null | tr '[:upper:]' '[:lower:]')
  arch=$(uname -m 2>/dev/null)

  case "$os" in
    linux) os='linux' ;;
    darwin) os='darwin' ;;
    *)
      printf '%s\n' "unsupported OS: $os (expected linux or darwin)"
      return 1
      ;;
  esac

  case "$arch" in
    x86_64|amd64) arch='amd64' ;;
    arm64|aarch64) arch='arm64' ;;
    *)
      printf '%s\n' "unsupported architecture: $arch (expected amd64 or arm64)"
      return 1
      ;;
  esac

  printf '%s %s\n' "$os" "$arch"
}

lowercase() {
  printf '%s' "$1" | tr '[:upper:]' '[:lower:]'
}

# Prints the lowercase SHA-256 of a file, using whichever digest tool the
# machine has. Exit status: 0 with the digest on stdout, 2 if a tool was found
# but could not produce a digest, 3 if no tool exists at all.
#
# This is called inside a command substitution, so it must never call fail():
# the exit would only end the subshell. The caller maps the status to a
# message, which is what keeps "your machine is missing something" apart from
# "these bytes are not the published ones".
#
# python3 comes last on purpose. macOS ships a /usr/bin/python3 stub until the
# Command Line Tools are installed: `command -v python3` finds it, but running
# it pops a GUI prompt and exits non-zero. Keeping shasum and openssl, both of
# which macOS ships working, ahead of it makes that branch unreachable there.
#
# The three shell backends read the archive from stdin instead of being given
# its path, so no filename can end up in their output. GNU coreutils escapes
# odd characters in the name it echoes back and marks the whole line with a
# leading backslash; since the temporary directory comes from mktemp, which
# honours a user-controlled TMPDIR, that backslash could otherwise glue itself
# to the digest and read as tampering. None of these run in a pipeline either,
# so the tool's own exit status survives. Their stderr is left alone as well,
# so whatever the tool has to say about its own failure reaches the user.
compute_sha256() {
  digest_file=$1

  if command -v sha256sum >/dev/null 2>&1; then
    # Reading stdin, the output is "<digest>  -", so take the first field.
    digest_out=$(sha256sum <"$digest_file") || return 2
    digest_value=${digest_out%% *}
  elif command -v shasum >/dev/null 2>&1; then
    digest_out=$(shasum -a 256 <"$digest_file") || return 2
    digest_value=${digest_out%% *}
  elif command -v openssl >/dev/null 2>&1; then
    # "SHA2-256(stdin)= <digest>" on OpenSSL 3, "SHA256(stdin)= <digest>" on
    # 1.x, so take the last field and let the label drift where it likes.
    digest_out=$(openssl dgst -sha256 <"$digest_file") || return 2
    digest_value=${digest_out##* }
  elif command -v python3 >/dev/null 2>&1; then
    # python3 prints the digest alone, so it can take the path directly.
    digest_value=$(python3 -c 'import hashlib,sys; print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$digest_file") || return 2
  else
    return 3
  fi

  # A backend that exits 0 but prints something that is not a digest is a
  # broken tool, not a bad download, so report it as such.
  case $digest_value in
    '' | *[!0-9a-fA-F]*) return 2 ;;
  esac
  [ ${#digest_value} -eq 64 ] || return 2

  lowercase "$digest_value"
}

verify_checksum() {
  archive_path=$1
  checksums_path=$2
  archive_name=$3
  archive_url=$4

  expected=$(awk -v name="$archive_name" '$2 == name { print $1; exit }' "$checksums_path")
  [ -n "$expected" ] || fail "could not find checksum for ${archive_name}"
  # goreleaser writes lowercase and every backend above agrees, but the
  # Windows installer normalises case on both sides and this one should too.
  expected=$(lowercase "$expected")

  actual=$(compute_sha256 "$archive_path")
  status=$?

  # The temporary directory is deleted on exit, so anyone who lands on one of
  # these failures cannot inspect the download afterwards. Print what they
  # need to repeat the check by hand.
  if [ "$status" -eq 3 ]; then
    # Refusing here is deliberate. This installs a binary that runs as a
    # privileged daemon, so installing bytes nobody checked is worse than not
    # installing at all, and continuing with a warning meant nobody ever saw
    # it.
    fail "refusing to install ${archive_name}: none of sha256sum, shasum, openssl or python3 is available to check it against the digest published with the release. Install one of them (for example the coreutils package) and run this again, or check it yourself:
  archive:  ${archive_url}
  expected: ${expected}"
  fi

  if [ "$status" -ne 0 ]; then
    fail "could not compute a sha256 digest for ${archive_name}. The digest tool on this machine failed or the downloaded file could not be read; this does not mean the download was altered."
  fi

  if [ "$actual" != "$expected" ]; then
    fail "${archive_name} does not match the digest published with the release. A truncated or stale copy from a proxy or CDN looks like this too, so retry before assuming the worst:
  archive:  ${archive_url}
  expected: ${expected}
  actual:   ${actual}"
  fi
}

# Each of these is used somewhere with no fallback: awk pulls the expected
# digest out of checksums.txt, tr folds the output of uname and the digests to
# lowercase, and sed trims the server URL before it is handed to the binary.
# Missing, they do not announce themselves: they leave an empty string behind
# and the script fails later with an error that blames the wrong thing.
need_cmd curl
need_cmd tar
need_cmd awk
need_cmd tr
need_cmd sed

install_dir=$(resolve_install_dir) || fail "$install_dir"
mkdir -p "$install_dir" || fail "failed to create install dir: $install_dir"
[ -w "$install_dir" ] || fail "install dir is not writable: $install_dir"

platform=$(resolve_platform) || fail "$platform"
# Unquoted on purpose: resolve_platform prints "<os> <arch>" for splitting.
set -- $platform
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
verify_checksum "$archive_path" "$checksums_path" "$archive_name" "$archive_url"

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
