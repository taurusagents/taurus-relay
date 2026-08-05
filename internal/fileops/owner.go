package fileops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Taurus lays the node data root out as
//
//	<data-root>/taurus-drives/<userId>/<agentId>/{workspace,shared,taurus,...}
//	<data-root>/taurus-drives-trash/<epochMs>__<userId>__<agentId>/
//
// Both roots are mirrored on the daemon side (see src/daemon/container-nodes.ts
// and src/daemon/drive-trash.ts); the names below must stay in lockstep with them.
const (
	DrivesDirName      = "taurus-drives"
	DrivesTrashDirName = "taurus-drives-trash"
)

// DriveOwnerNone is the explicit "this node does not use Docker userns-remap"
// opt-out for --drive-owner / TAURUS_DRIVE_OWNER. It is deliberately a value the
// operator has to type: an *unset* variable is treated as a misconfiguration and
// refuses to start node mode, because on a remapped node "unset" silently
// produces drive directories the agent container cannot write.
const DriveOwnerNone = "none"

// DriveOwner is the uid/gid pair that must own everything the relay creates
// under a managed drive root.
//
// Why this exists: Taurus container nodes run dockerd with `userns-remap`, so a
// container's root user is a *different* host uid — the remap base, normally
// 100000:100000. The relay process itself runs as an ordinary unix user, so any
// directory it creates under an agent's drive would be owned by the relay and
// therefore unwritable by the agent inside the container. Handing every created
// component to the remap base gives one identity authority over the whole
// subtree, which is the ownership model adjudicated in
// kb/plans/userns-remap-ownership-adjudication-2026-08-03.md.
type DriveOwner struct {
	UID int
	GID int
}

func (o DriveOwner) String() string { return strconv.Itoa(o.UID) + ":" + strconv.Itoa(o.GID) }

var (
	driveOwnerMu    sync.RWMutex
	driveOwner      *DriveOwner
	driveOwnedRoots []string
)

// errNotDirectory mirrors what os.MkdirAll reports when an existing path
// component is not a directory.
var errNotDirectory = syscall.ENOTDIR

// lchownPath and fchownFile are the only two places in the relay that change
// ownership. They are variables so tests can observe the exact set of paths the
// choke point hands over — including unprivileged CI, where a real chown to the
// userns-remap base would be EPERM — and so a chown failure can be injected to
// exercise the fail-closed startup path.
var (
	lchownPath = os.Lchown
	// fchownFile takes the logical destination path as well as the handle: the
	// handle is usually a temp file that is about to be renamed into place, and
	// the path is what the caller (and the test recorder) reasons about.
	fchownFile = func(f *os.File, path string, uid, gid int) error { return f.Chown(uid, gid) }
)

// DriveRoots returns the drive subtrees whose contents belong to the drive
// owner. Files the relay writes elsewhere under the data root (the managed
// seccomp profile, for example) are host-side artifacts that dockerd reads as
// root and are deliberately left alone.
func DriveRoots(dataPath string) []string {
	clean := filepath.Clean(dataPath)
	return []string{
		filepath.Join(clean, DrivesDirName),
		filepath.Join(clean, DrivesTrashDirName),
	}
}

// ParseDriveOwner turns the configured --drive-owner / TAURUS_DRIVE_OWNER value
// into an owner. It returns (nil, nil) only for the explicit "none" opt-out.
//
// The value is never sniffed from /etc/subuid or `docker info`: a wrong guess
// here corrupts ownership of the whole drive tree, so the remap base is an
// explicit deployment input that the daemon independently cross-checks against
// the node's docker userns-remap probe.
func ParseDriveOwner(raw string) (*DriveOwner, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New(
			`drive owner is not configured: set --drive-owner (or TAURUS_DRIVE_OWNER) to the Docker userns-remap base as "<uid>:<gid>" (normally 100000:100000, matching "Docker Root Dir: /var/lib/docker/100000.100000"), or to "none" on nodes where dockerd does not use userns-remap`)
	}
	if strings.EqualFold(trimmed, DriveOwnerNone) {
		return nil, nil
	}

	uidText, gidText, ok := strings.Cut(trimmed, ":")
	if !ok {
		return nil, fmt.Errorf("invalid drive owner %q: expected \"<uid>:<gid>\" or %q", raw, DriveOwnerNone)
	}
	uid, err := parseOwnerID(uidText)
	if err != nil {
		return nil, fmt.Errorf("invalid drive owner %q: uid: %w", raw, err)
	}
	gid, err := parseOwnerID(gidText)
	if err != nil {
		return nil, fmt.Errorf("invalid drive owner %q: gid: %w", raw, err)
	}
	return &DriveOwner{UID: uid, GID: gid}, nil
}

func parseOwnerID(text string) (int, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0, errors.New("must not be empty")
	}
	// Reject anything that is not plain decimal digits: strconv would happily
	// accept "+100000" / "-1", and a signed id here is always a typo.
	for _, r := range trimmed {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%q is not a non-negative decimal id", text)
		}
	}
	value, err := strconv.ParseInt(trimmed, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%q is out of range for a unix id", text)
	}
	return int(value), nil
}

// SetDriveOwnership installs the owner and the roots it applies to. Passing a
// nil owner disables chowning entirely, which is what connect-mode relays and
// unremapped nodes ("none") run with.
func SetDriveOwnership(owner *DriveOwner, roots []string) {
	driveOwnerMu.Lock()
	defer driveOwnerMu.Unlock()

	driveOwner = owner
	driveOwnedRoots = nil
	if owner == nil {
		return
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		driveOwnedRoots = append(driveOwnedRoots, filepath.Clean(root))
	}
}

// ConfiguredDriveOwner reports the installed owner (nil when chowning is off).
func ConfiguredDriveOwner() *DriveOwner {
	driveOwnerMu.RLock()
	defer driveOwnerMu.RUnlock()
	return driveOwner
}

// DriveOwnerSetting renders the configured owner the way it is published to the
// daemon in node registration metadata.
func DriveOwnerSetting() string {
	if owner := ConfiguredDriveOwner(); owner != nil {
		return owner.String()
	}
	return DriveOwnerNone
}

// ownerFor returns the owner that applies to a path, or nil when the path is
// outside every managed drive root.
func ownerFor(path string) *DriveOwner {
	driveOwnerMu.RLock()
	defer driveOwnerMu.RUnlock()

	if driveOwner == nil {
		return nil
	}
	clean := filepath.Clean(path)
	for _, root := range driveOwnedRoots {
		if clean == root || strings.HasPrefix(clean, root+string(filepath.Separator)) {
			return driveOwner
		}
	}
	return nil
}

// chownCreatedPath hands a path the relay just created to the drive owner.
// Lchown, never Chown: the relay must never follow a symlink while changing
// ownership, even one it believes it just created itself.
func chownCreatedPath(path string) error {
	owner := ownerFor(path)
	if owner == nil {
		return nil
	}
	if err := lchownPath(path, owner.UID, owner.GID); err != nil {
		return fmt.Errorf("chown %s to %s: %w", path, owner, err)
	}
	return nil
}

// chownCreatedFile is the open-file-handle form: fchown(2) cannot be redirected
// by a rename or symlink swap between create and chown.
func chownCreatedFile(f *os.File, path string) error {
	owner := ownerFor(path)
	if owner == nil {
		return nil
	}
	if err := fchownFile(f, path, owner.UID, owner.GID); err != nil {
		return fmt.Errorf("chown %s to %s: %w", path, owner, err)
	}
	return nil
}

// EnsureOwnedDir creates path plus any missing parent directories and hands
// *every component it creates* to the drive owner.
//
// Chowning only the leaf is not good enough and the failure it produces is
// silent: the agent can write inside the leaf, so a naive "can the new agent
// write?" check passes, but it cannot create siblings because the intermediate
// directories still belong to the relay. Components that already exist are left
// exactly as they are — the relay owns what it creates and nothing else, so a
// stray path under the drive root can never make it rewrite ownership of an
// existing tree.
//
// Modes:
//   - mode 0 means "the 0755 default, filtered by the process umask" — exactly
//     what os.MkdirAll(path, 0755) did before, so a user's connect-mode relay
//     with a restrictive umask keeps creating restrictive directories;
//   - a non-zero mode is applied exactly (codex-config 0700 means 0700);
//   - **inside a managed drive root the mode is always applied exactly**, umask
//     or not. Drive directory permissions are part of the ownership contract the
//     container depends on, and must not silently depend on the relay unit's
//     UMask= setting.
func EnsureOwnedDir(path string, mode os.FileMode) error {
	callerSetMode := mode != 0
	if mode == 0 {
		mode = 0o755
	}

	if info, err := os.Lstat(path); err == nil {
		if info.IsDir() {
			return nil
		}
		// os.MkdirAll, which this replaced, follows a final symlink and succeeds
		// when it points at a directory. Connect-mode relays run against real
		// user home directories where that is ordinary (a symlinked project dir),
		// so keep the behavior there.
		//
		// Inside a managed drive root it stays a hard error: nothing in the drive
		// layout is legitimately a symlink, so one appearing at the leaf between
		// ValidatePath resolving the path and this call is a swap attempt, and
		// following it would widen exactly the surface #358 is about.
		if info.Mode()&os.ModeSymlink != 0 && ownerFor(path) == nil {
			if target, statErr := os.Stat(path); statErr == nil && target.IsDir() {
				return nil
			}
		}
		return &os.PathError{Op: "mkdir", Path: path, Err: errNotDirectory}
	} else if !os.IsNotExist(err) {
		return err
	}

	missing, err := missingDirComponents(path)
	if err != nil {
		return err
	}

	// missingDirComponents returns deepest-first; create shallowest-first.
	for i := len(missing) - 1; i >= 0; i-- {
		component := missing[i]
		componentMode := os.FileMode(0o755)
		isLeaf := component == filepath.Clean(path)
		if isLeaf {
			componentMode = mode
		}

		// ⚠️ Component-by-component creation has a swap window that a single
		// mkdirat(2) does not: between creating component i and creating i+1,
		// anything with write access to component i-1 could replace component i
		// with a symlink, and i+1 would then be created through it. Nothing
		// exploits that today — every path the daemon sends here is either a
		// pre-container path or inside a directory no container can write (see
		// the invariant recorded at the daemon's file.mkdir/file.write call
		// sites) — and closing it properly means resolving through openat2 with
		// RESOLVE_IN_ROOT and creating with mkdirat(2) relative to a pinned
		// dirfd (#358). Do not widen the set of callers without that.
		if err := os.Mkdir(component, componentMode); err != nil {
			if os.IsExist(err) {
				// Lost a race with a concurrent create (two launches for the
				// same user hit <userId>/ at once). Whoever created it applied
				// its own mode/chown; do not touch a path we did not create.
				continue
			}
			return err
		}
		// mkdir(2) masks the mode with the process umask, so an explicit chmod is
		// the only way to get the mode that was actually asked for.
		if (isLeaf && callerSetMode) || ownerFor(component) != nil {
			if err := os.Chmod(component, componentMode); err != nil {
				return err
			}
		}
		if err := chownCreatedPath(component); err != nil {
			return err
		}
	}
	return nil
}

// missingDirComponents lists the components of path that do not exist yet,
// deepest first, stopping at the first existing ancestor.
func missingDirComponents(path string) ([]string, error) {
	var missing []string
	current := filepath.Clean(path)
	for {
		_, err := os.Lstat(current)
		if err == nil {
			return missing, nil
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
		missing = append(missing, current)

		parent := filepath.Dir(current)
		if parent == current {
			// Reached the filesystem root without finding anything that exists.
			return missing, nil
		}
		current = parent
	}
}

// EnsureOwnedFile creates path as an empty regular file if it is missing and
// hands it to the drive owner, returning whether it created the file.
//
// If the path already exists its bytes and mode are left completely alone — this
// is the codex auth.json bootstrap path, so clobbering it would destroy live
// credentials — but its *ownership is verified*. A file left behind by a relay
// that predates drive ownership (or by a partial migration) is owned by the
// relay's own uid, and the container that bind-mounts it at 0600 then cannot
// read its own credentials: exactly the silent breakage this change exists to
// remove. Report it instead of repairing it, because a wrongly-owned credential
// file means the tree was not migrated and quietly chowning one file would hide
// that.
func EnsureOwnedFile(path string, mode os.FileMode) (bool, error) {
	if mode == 0 {
		mode = 0o600
	}
	if err := EnsureOwnedDir(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|oNoFollow, mode)
	if err != nil {
		// O_EXCL reports EEXIST for an existing regular file, but O_NOFOLLOW
		// reports ELOOP for an existing symlink, so classify by lstat rather
		// than by errno.
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return false, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return false, fmt.Errorf("%s must be a regular file, not a symlink", path)
		}
		if !info.Mode().IsRegular() {
			return false, fmt.Errorf("%s must be a regular file", path)
		}
		if err := verifyExistingOwner(path, info); err != nil {
			return false, err
		}
		return false, nil
	}
	defer f.Close()

	// open(2) masks the create mode with the umask, same as mkdir(2).
	if err := f.Chmod(mode); err != nil {
		return true, err
	}
	if err := chownCreatedFile(f, path); err != nil {
		return true, err
	}
	return true, nil
}

// verifyExistingOwner fails when a pre-existing path inside a managed drive root
// is not owned by the configured drive owner. Outside the drive roots (connect
// mode, or the node's own runtime staging paths) ownership is not ours to have
// an opinion about, so it is not checked.
func verifyExistingOwner(path string, info os.FileInfo) error {
	owner := ownerFor(path)
	if owner == nil {
		return nil
	}
	uid, gid, err := fileOwner(info)
	if err != nil {
		return nil // platform without unix ownership; nothing to verify
	}
	if uid != owner.UID || gid != owner.GID {
		return fmt.Errorf(
			"%s already exists but is owned by %d:%d instead of the configured drive owner %s; "+
				"either the drive tree was not migrated (chown -R %s ...), or a container rewrote it "+
				"under a different in-container uid (container root owns this path and may chown it)",
			path, uid, gid, owner, owner)
	}
	return nil
}
