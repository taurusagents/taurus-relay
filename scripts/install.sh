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

# Removes everything this script creates that must not outlive it: the
# temporary download directory, and the staged binary if a run ended between
# writing it and renaming it into place. It reads tmpdir and binary_tmp, so it
# must not be installed as a trap before both of those are assigned.
cleanup() {
  rm -rf "$tmpdir"
  rm -f "$binary_tmp"
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

  # Capture the uid and check it is a number before comparing. `[ "" -eq 0 ]`
  # is an error rather than a false, and an error here is simply a branch not
  # taken: the root check would silently fall through to the per-user path and
  # a root install would land somewhere nobody asked for and report success.
  uid=$(id -u) || uid=''
  case $uid in
    '' | *[!0-9]*)
      printf '%s\n' 'could not read the current user id from id -u; set TAURUS_INSTALL_DIR explicitly'
      return 1
      ;;
  esac

  if [ "$uid" -eq 0 ]; then
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

  # Detection producing nothing is not the same as detecting something we do
  # not support. Reporting it as "unsupported OS: " tells a customer their
  # system is not supported when what actually happened is that uname or tr
  # did not answer, and there is nothing in that message they can act on.
  if [ -z "$os" ]; then
    printf '%s\n' 'could not detect the operating system: "uname -s | tr" produced nothing. Check that uname and tr work on this machine.'
    return 1
  fi
  if [ -z "$arch" ]; then
    printf '%s\n' 'could not detect the machine architecture: "uname -m" produced nothing. Check that uname works on this machine.'
    return 1
  fi

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

# is_hex64 is the shape a SHA-256 digest has however it is written; is_sha256
# is the shape it has after case has been folded, which is the only form the
# comparison in verify_checksum ever sees.
#
# Every value on its way to that comparison is checked, before and after the
# fold, because two empty strings compare equal and the comparison is the only
# thing between the caller and bytes nobody checked. Checking on both sides of
# the fold also says which step went wrong: a value that was never a digest
# came from the file or the digest tool, while one that was a digest and is not
# any more was mangled by the fold.
is_hex64() {
  case $1 in
    *[!0-9a-fA-F]*) return 1 ;;
  esac
  [ ${#1} -eq 64 ]
}

is_sha256() {
  case $1 in
    *[!0-9a-f]*) return 1 ;;
  esac
  [ ${#1} -eq 64 ]
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
# stdout, 2 if a digest tool was found but could not produce a digest, 3 if no
# digest tool exists at all, 4 if a digest was produced and then lost folding
# its case.
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
# honours a user-controlled TMPDIR, that backslash would otherwise be glued to
# the digest and this would refuse to install on any machine whose TMPDIR
# contains a backslash or a newline. None of these run in a pipeline either,
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
  # broken tool, not a bad download.
  is_hex64 "$digest_value" || return 2

  # Fold case, then check again, then print the value that was checked. The
  # order matters: validating one value and emitting another leaves the caller
  # holding something nothing ever looked at, and a fold that returned success
  # without returning the string would hand back an empty digest with a success
  # status. The fold gets its own status because it is a different tool, and
  # blaming the digest backend for what tr did would be a lie.
  digest_value=$(lowercase "$digest_value") || return 4
  is_sha256 "$digest_value" || return 4

  printf '%s\n' "$digest_value"
}

# Both digests are folded to lowercase with tr before they are compared, so tr
# is a second tool that can fail on its own. Saying so is worth a message of its
# own: naming the digest backend here would blame the wrong thing, and a fold
# that returns success without returning the string leaves a value that would
# compare equal to any other empty one.
#
# Arguments: the URL the archive came from, and the digest published for it.
fail_fold() {
  fail "could not fold a sha256 digest to lowercase: tr failed, or returned something other than the digest it was given. This usually means a problem with this machine rather than with the download. To check the download somewhere else:
  archive:  ${1}
  expected: ${2}"
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

  published=$(awk -v name="$archive_name" '$2 == name { print $1; exit }' "$checksums_path")
  awk_status=$?

  # A checksums file we could not read is a different problem from a checksums
  # file that simply has no line for this archive. An empty one is what a
  # truncated copy looks like, and reporting that as a missing checksum sends
  # the user to complain about the release instead of retrying.
  if [ "$awk_status" -ne 0 ] || [ ! -s "$checksums_path" ]; then
    fail "the checksums file downloaded for this release is empty or could not be read. A truncated or stale copy from a proxy or CDN looks like this, so a retry is worth trying:
  checksums: ${checksums_url}"
  fi

  if [ -z "$published" ]; then
    fail "the checksums file downloaded for this release has no entry for ${archive_name}:
  checksums: ${checksums_url}"
  fi

  # A line that exists but whose digest field is not a digest means the file we
  # received is corrupt, so there is nothing to check the download against. The
  # field is deliberately not echoed back: until this test passes it is
  # arbitrary bytes from the network, and the URL is enough to go and look.
  if ! is_hex64 "$published"; then
    fail "the entry for ${archive_name} in the checksums file downloaded for this release is not a sha256 digest, so there is nothing to check the download against. A corrupted copy looks like this:
  checksums: ${checksums_url}"
  fi

  # goreleaser writes lowercase and every backend above agrees, but the Windows
  # installer normalises case on both sides and this one should too. Past this
  # point the published digest is known to be a digest, so if the fold returns
  # something else it is the fold that broke, not the file.
  expected=$(lowercase "$published")
  is_sha256 "$expected" || fail_fold "$archive_url" "$published"

  actual=$(compute_sha256 "$archive_path")
  status=$?

  if [ "$status" -eq 4 ]; then
    fail_fold "$archive_url" "$published"
  fi

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
    # Any status other than 3 or 4 means a tool was found and then could not
    # produce a digest, so ask which one it was and name it: "install
    # something" is not actionable, "sha256sum failed" is.
    digest_tool=$(sha256_backend) || digest_tool='the digest tool'
    fail "could not compute a sha256 digest for ${archive_name}: ${digest_tool} failed, or the file could not be read. This usually means a problem with this machine rather than with the download. To check the download somewhere else:
  archive:  ${archive_url}
  expected: ${expected}"
  fi

  # The shape of the computed digest is checked here as well as inside
  # compute_sha256, so that the comparison below depends on nothing but itself.
  # It gets its own branch rather than sharing the one above, because reaching
  # it means compute_sha256 reported success and returned something that is not
  # a digest: no tool misbehaved, this script did, and naming a tool here would
  # be the same false accusation this check exists to prevent. Nowhere to send
  # the reader either, so it says what it knows and stops.
  if ! is_sha256 "$actual"; then
    fail "the installer computed something that is not a sha256 digest for ${archive_name}, so it cannot check the download. This is a fault in the installer itself rather than in the download or in any tool on this machine:
  archive:  ${archive_url}
  expected: ${expected}"
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
# (choose the install directory and the platform) leave an empty string behind
# when they are missing, and the script would carry on with an empty OS name,
# an empty digest, or quietly the wrong install directory. mktemp is here
# because the check further down that catches it failing can only explain the
# failure it was written for, a TMPDIR that does not work; a machine with no
# mktemp at all would be sent to look at TMPDIR for nothing.
#
# mkdir, cp, mv and chmod are deliberately not listed: each failure site names
# the step it belongs to, so a check up here would say the same thing slightly
# earlier and nothing more. Decompression helpers such as gzip are left out for
# a different reason - whether one is even needed depends on the tar
# implementation, since BusyBox and bsdtar decompress internally, so requiring
# it would turn away machines that work; when GNU tar does need it, it names it
# on the line above ours.
need_cmd curl
need_cmd tar
need_cmd awk
need_cmd tr
need_cmd sed
need_cmd id
need_cmd uname
need_cmd mktemp

install_dir=$(resolve_install_dir) || fail "$install_dir"
mkdir -p "$install_dir" || fail "failed to create install dir: $install_dir"
[ -w "$install_dir" ] || fail "install dir is not writable: $install_dir"

binary_path="$install_dir/taurus-relay"
# The binary is written under a temporary name in the install directory and
# renamed into place. The name is hidden and carries this shell's pid, so two
# installers running at once cannot collide on it. Both names are settled here
# rather than at the point of use, so that cleanup() has everything it reads
# before any trap referring to it is armed.
binary_tmp="$install_dir/.taurus-relay.$$"

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
#
# The second form exists for the BSD and macOS mktemp. Both attempts are kept
# quiet, unlike the digest tools further up: GNU coreutils rejects that second
# template outright with "too few X's", which is a complaint about this script
# rather than about anything the reader can fix, and it would print on every
# Linux machine that reaches here.
tmpdir=$(mktemp -d 2>/dev/null || mktemp -d -t taurus-relay-installer 2>/dev/null) || tmpdir=''
[ -n "$tmpdir" ] && [ -d "$tmpdir" ] ||
  fail "failed to create a temporary directory (TMPDIR is ${TMPDIR:-unset}); set TMPDIR to an existing writable directory"

# Armed as soon as there is a directory to remove. The gap between mktemp
# returning and the first trap being installed is not closable: a shell cannot
# create a resource and arm its handler in one indivisible step, so a signal
# landing in that gap still leaves the directory behind. Keeping the gap to
# assignments with no I/O in them is as far as this goes.
#
# The exit trap covers every ordinary end. Signals need handlers that exit,
# because a handler that simply returns lets the script continue from where it
# was interrupted: a Ctrl-C between staging the binary and starting it would
# otherwise delete the temporary directory and then run the daemon anyway.
# 128 plus the signal number is the status a shell reports for a process that
# was killed by that signal.
trap cleanup 0
trap 'cleanup; exit 129' HUP
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

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

# The staged file is removed by cleanup() on every failure this script can
# catch. A kill that cannot be trapped - SIGKILL, the OOM killer, power loss -
# in the moment between the copy and the rename can still leave one behind. It
# is inert: hidden, never executed, and never named by anything here. Sweeping
# for leftovers on startup is deliberately not done, because a stale staged
# file is indistinguishable from one a concurrently running installer is about
# to rename into place, and deleting that would break a second install running
# alongside this one.
cp "$extract_dir/taurus-relay" "$binary_tmp" ||
  fail "failed to install taurus-relay into ${install_dir}: could not write the new binary (staged at ${binary_tmp})"
chmod 0755 "$binary_tmp" ||
  fail "failed to install taurus-relay into ${install_dir}: could not make the new binary executable (staged at ${binary_tmp})"

# mv -f onto an existing directory moves the staged file inside it and reports
# success, which would end with this script announcing an install that did not
# happen. Nothing else here would notice: the announcement, the PATH note and
# the exit status would all be those of a successful run.
if [ -d "$binary_path" ]; then
  fail "cannot install taurus-relay: ${binary_path} is a directory. Remove it, or set TAURUS_INSTALL_DIR to somewhere else."
fi

# One rename, so anyone looking at the destination sees either the previous
# binary or the new one and never a half-written file. It is also the only way
# to replace a binary that is currently running: writing onto it fails with
# ETXTBSY, and re-running this installer while the relay is up is the ordinary
# way to upgrade.
mv -f "$binary_tmp" "$binary_path" || fail "failed to install ${binary_path}"

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
taurus_url_lower=$(lowercase "$TAURUS_URL")

set -- connect --token "$TAURUS_TOKEN" --server "$TAURUS_URL"
case "$taurus_url_lower" in
  http://*)
    log "Warning: ${TAURUS_URL} is non-TLS; passing --insecure to taurus-relay connect."
    set -- connect --insecure --token "$TAURUS_TOKEN" --server "$TAURUS_URL"
    ;;
esac

# exec replaces this process, so the exit trap will never run. Without this,
# every successful install leaves the archive and a second copy of the daemon
# binary behind under TMPDIR. The traps stay armed for the case where exec
# itself fails; removing an already-removed path is not an error.
cleanup

log "Starting taurus-relay connect against ${TAURUS_URL}..."
exec "$binary_path" "$@"
