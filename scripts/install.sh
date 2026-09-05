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

# Names the digest tool this machine will use, or prints nothing and returns 1
# if it has none. It is the single place the preference order is written down,
# so compute_sha256 and the message that has to name the tool cannot disagree.
#
# python3 comes last on purpose. macOS ships a /usr/bin/python3 stub until the
# Command Line Tools are installed: `command -v python3` finds it, but running
# it pops a GUI prompt and exits non-zero. Keeping shasum and openssl, both of
# which macOS ships working, ahead of it makes that branch unreachable there.
sha256_backend() {
  for backend_candidate in sha256sum shasum openssl python3; do
    if command -v "$backend_candidate" >/dev/null 2>&1; then
      printf '%s\n' "$backend_candidate"
      return 0
    fi
  done
  return 1
}

# Prints the lowercase SHA-256 of a file. Exit status: 0 with the digest on
# stdout, 2 if a tool was found but could not produce a digest, 3 if no tool
# exists at all.
#
# This is called inside a command substitution, so it must never call fail():
# the exit would only end the subshell. The caller maps the status to a
# message, which is what keeps "your machine is missing something" apart from
# "these bytes are not the published ones".
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

  digest_tool=$(sha256_backend) || return 3

  case $digest_tool in
    sha256sum)
      # Reading stdin, the output is "<digest>  -", so take the first field.
      digest_out=$(sha256sum <"$digest_file") || return 2
      digest_value=${digest_out%% *}
      ;;
    shasum)
      digest_out=$(shasum -a 256 <"$digest_file") || return 2
      digest_value=${digest_out%% *}
      ;;
    openssl)
      # "SHA2-256(stdin)= <digest>" on OpenSSL 3, "SHA256(stdin)= <digest>" on
      # 1.x, so take the last field and let the label drift where it likes.
      digest_out=$(openssl dgst -sha256 <"$digest_file") || return 2
      digest_value=${digest_out##* }
      ;;
    python3)
      # python3 prints the digest alone, so it can take the path directly.
      digest_value=$(python3 -c 'import hashlib,sys; print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$digest_file") || return 2
      ;;
    *)
      # Only reachable if a name is added to the list above without an arm
      # here; say the tool failed rather than fall through with no digest.
      return 2
      ;;
  esac

  # A backend that exits 0 but prints something that is not a digest is a
  # broken tool, not a bad download, so report it as such.
  case $digest_value in
    '' | *[!0-9a-fA-F]*) return 2 ;;
  esac
  [ ${#digest_value} -eq 64 ] || return 2

  lowercase "$digest_value"
}

# Arguments: the downloaded archive, the downloaded checksums file, the
# archive's asset name, and the two URLs they came from. The URLs are here
# because a trap deletes the temporary directory on exit, so every failure
# below has to hand the user enough to repeat the check by hand.
verify_checksum() {
  archive_path=$1
  checksums_path=$2
  archive_name=$3
  archive_url=$4
  checksums_url=$5

  expected=$(awk -v name="$archive_name" '$2 == name { print $1; exit }' "$checksums_path")
  awk_status=$?

  # A checksums file we could not read is a different problem from a checksums
  # file that simply has no line for this archive. An empty one is what a
  # truncated copy looks like, and reporting that as a missing checksum sends
  # the user to complain about the release instead of retrying.
  if [ "$awk_status" -ne 0 ] || [ ! -s "$checksums_path" ]; then
    fail "the checksums file downloaded for this release is empty or could not be read. A truncated or stale copy from a proxy or CDN looks like this, so a retry is worth trying:
  checksums: ${checksums_url}"
  fi

  if [ -z "$expected" ]; then
    fail "the checksums file downloaded for this release has no entry for ${archive_name}:
  checksums: ${checksums_url}"
  fi

  # goreleaser writes lowercase and every backend above agrees, but the
  # Windows installer normalises case on both sides and this one should too.
  expected=$(lowercase "$expected")

  actual=$(compute_sha256 "$archive_path")
  status=$?

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
    # Any status other than 3 means a tool was found and then could not produce
    # a digest, so ask which one it was and name it: "install something" is not
    # actionable, "sha256sum failed" is.
    digest_tool=$(sha256_backend) || digest_tool='the digest tool'
    fail "could not compute a sha256 digest for ${archive_name}: ${digest_tool} failed, or the file could not be read. This usually means a problem with this machine rather than with the download."
  fi

  if [ "$actual" != "$expected" ]; then
    fail "${archive_name} does not match the digest published with the release. A truncated or stale copy from a proxy or CDN looks like this too, so retry before assuming the worst:
  archive:  ${archive_url}
  expected: ${expected}
  actual:   ${actual}"
  fi
}

# Declared here are the commands whose absence would be reported as somebody
# else's fault. Missing curl and tar surface as "failed to download" and
# "failed to extract", which point at the release rather than at this machine.
# awk (reads the expected digest out of checksums.txt), tr (folds the output of
# uname and the digests to lowercase), sed (trims the server URL), id and uname
# (choose the install directory and the platform) do not even produce an error:
# they leave an empty string behind, and the script carries on with an empty OS
# name, an empty digest, or quietly the wrong install directory.
#
# mkdir, cp, chmod and mktemp are deliberately not listed. Their own failure
# already names the operation that failed on this machine, so there is nothing
# for a check up here to add.
need_cmd curl
need_cmd tar
need_cmd awk
need_cmd tr
need_cmd sed
need_cmd id
need_cmd uname

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

# mktemp honours TMPDIR, so a TMPDIR naming a directory that does not exist
# makes both attempts fail. Unchecked, tmpdir would be empty and every path
# built from it would start at the filesystem root: as root the download and
# the extracted tree land in /, the trap's rm -rf deletes nothing, and the run
# reports a successful install.
tmpdir=$(mktemp -d 2>/dev/null || mktemp -d -t taurus-relay-installer) || tmpdir=''
[ -n "$tmpdir" ] && [ -d "$tmpdir" ] ||
  fail "failed to create a temporary directory (TMPDIR is ${TMPDIR:-unset}); set TMPDIR to an existing writable directory"
trap 'rm -rf "$tmpdir"' 0 HUP INT TERM

archive_path="$tmpdir/$archive_name"
checksums_path="$tmpdir/checksums.txt"
extract_dir="$tmpdir/extract"
mkdir -p "$extract_dir" || fail "failed to create ${extract_dir}"

log "Downloading ${archive_name} (${release_label})..."
curl -fsSL "$archive_url" -o "$archive_path" || fail "failed to download ${archive_url}"
curl -fsSL "$checksums_url" -o "$checksums_path" || fail "failed to download ${checksums_url}"
verify_checksum "$archive_path" "$checksums_path" "$archive_name" "$archive_url" "$checksums_url"

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
[ -n "$TAURUS_URL" ] || fail 'TAURUS_URL is empty once trailing slashes are trimmed; set it to the full Taurus server URL, for example https://app.taurusagents.com'

# URL schemes are case-insensitive, so HTTP:// is just as plaintext as http://
# and has to raise the same warning. Only the comparison uses the lowercased
# copy; what reaches the binary is the string the user gave us.
taurus_url_scheme=$(lowercase "$TAURUS_URL")

set -- connect --token "$TAURUS_TOKEN" --server "$TAURUS_URL"
case "$taurus_url_scheme" in
  http://*)
    log "Warning: ${TAURUS_URL} is non-TLS; passing --insecure to taurus-relay connect."
    set -- connect --insecure --token "$TAURUS_TOKEN" --server "$TAURUS_URL"
    ;;
esac

log "Starting taurus-relay connect against ${TAURUS_URL}..."
exec "$binary_path" "$@"
