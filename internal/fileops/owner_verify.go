package fileops

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// driveOwnerProbeDirName is the scratch directory VerifyDriveOwnership creates
// and removes inside each managed drive root. The leading dot and the fixed name
// keep it out of the way of the <userId>/ directories Taurus creates there, and
// make a leftover after a hard kill obvious and safe to remove.
const driveOwnerProbeDirName = ".taurus-drive-owner-check"

// VerifyDriveOwnership proves on the real filesystem, at startup, that this
// relay process can (a) see each managed drive root owned by the configured
// owner and (b) create a directory there and hand it to that owner.
//
// This is deliberately empirical rather than a capability-bit check: the same
// authority can come from running as root, from systemd AmbientCapabilities, or
// from file capabilities on the binary, and an operator who upgrades the binary
// without re-applying `setcap` must fail loudly here rather than quietly create
// drive directories the agent container cannot write.
//
// It is a no-op when no drive owner is configured ("none" / connect mode).
func VerifyDriveOwnership() error {
	owner := ConfiguredDriveOwner()
	if owner == nil {
		return nil
	}

	driveOwnerMu.RLock()
	roots := append([]string(nil), driveOwnedRoots...)
	driveOwnerMu.RUnlock()

	if len(roots) == 0 {
		return fmt.Errorf("drive owner %s is configured but no drive roots were registered", owner)
	}

	for _, root := range roots {
		if err := verifyDriveRootOwnership(root, *owner); err != nil {
			return err
		}
	}
	return nil
}

func verifyDriveRootOwnership(root string, owner DriveOwner) error {
	// Creating a missing root is part of the check: EnsureOwnedDir chowns what it
	// creates, so a fresh node ends up with correctly owned roots and proves the
	// chown works in the same step.
	if err := EnsureOwnedDir(root, 0o755); err != nil {
		return fmt.Errorf("drive root %s is not usable: %w%s", root, err, processIdentityHint())
	}

	rootUID, rootGID, err := pathOwner(root)
	if err != nil {
		return fmt.Errorf("cannot stat drive root %s: %w", root, err)
	}
	if rootUID != owner.UID || rootGID != owner.GID {
		return fmt.Errorf(
			"drive root %s is owned by %d:%d but the configured drive owner is %s; "+
				"migrate the tree before starting the relay (`chown -R %s %s`) or fix --drive-owner / TAURUS_DRIVE_OWNER",
			root, rootUID, rootGID, owner, owner, root)
	}

	probe := filepath.Join(root, driveOwnerProbeDirName)
	// A probe dir left behind by a previous hard kill must not fail the check.
	if err := os.RemoveAll(probe); err != nil {
		return fmt.Errorf("cannot clear drive ownership probe %s: %w%s", probe, err, processIdentityHint())
	}
	defer os.RemoveAll(probe)

	if err := EnsureOwnedDir(probe, 0o755); err != nil {
		return fmt.Errorf(
			"drive ownership self-check failed: cannot create and chown %s to %s: %w%s",
			probe, owner, err, processIdentityHint())
	}
	probeUID, probeGID, err := pathOwner(probe)
	if err != nil {
		return fmt.Errorf("drive ownership self-check failed: cannot stat %s: %w", probe, err)
	}
	if probeUID != owner.UID || probeGID != owner.GID {
		return fmt.Errorf(
			"drive ownership self-check failed: %s came out owned by %d:%d instead of %s%s",
			probe, probeUID, probeGID, owner, processIdentityHint())
	}
	return nil
}

// processIdentityHint appends the effective ids and (on Linux) the effective
// capability mask, because "permission denied" while chowning is almost always
// a missing CAP_CHOWN/CAP_DAC_OVERRIDE grant in the systemd unit.
func processIdentityHint() string {
	hint := fmt.Sprintf(" [relay runs as uid=%d gid=%d", os.Geteuid(), os.Getegid())
	if caps := effectiveCapabilities(); caps != "" {
		hint += ", CapEff=" + caps
	}
	hint += "; node mode needs CAP_CHOWN, CAP_DAC_OVERRIDE and CAP_FOWNER (see the relay systemd unit) or root]"
	return hint
}

func effectiveCapabilities() string {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, "CapEff:"); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
