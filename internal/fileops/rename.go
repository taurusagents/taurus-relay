package fileops

import (
	"context"
	"fmt"
	"os"

	"github.com/taurusagents/taurus-relay/internal/protocol"
)

// Rename moves a path within the node's allowed roots.
func Rename(p *protocol.FileRenamePayload) error {
	return RenameContext(context.Background(), p)
}

// RenameContext backs the file.rename verb, which Taurus uses to move a deleted
// agent's whole drive directory into the sibling drive-trash root.
//
// It replaces a host-side `mv --no-copy -nT` for two reasons:
//
//   - Ownership. On a userns-remapped node the drive tree belongs to the remap
//     base, so a spawned `mv` running as the relay's own uid needs the relay's
//     capabilities to be inherited by children. Doing the rename in-process
//     removes that requirement entirely.
//   - Guarantees. This IS rename(2): it can never degrade into copy+delete the
//     way plain `mv` does across a filesystem or XFS quota-project boundary, so
//     EXDEV surfaces as a plain error instead of silently duplicating gigabytes.
//     That also drops the coreutils >= 9.4 (`--no-copy`) node requirement, the
//     "unrecognized option" special case, and the post-move re-stat the daemon
//     needed to detect `mv -n`'s silent no-op.
//
// The destination is never clobbered: on Linux that is enforced atomically by
// renameat2(RENAME_NOREPLACE) rather than by a check-then-act.
func RenameContext(ctx context.Context, p *protocol.FileRenamePayload) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	from, err := ValidatePath(p.From)
	if err != nil {
		return err
	}
	to, err := ValidatePath(p.To)
	if err != nil {
		return err
	}
	if err := checkContext(ctx); err != nil {
		return err
	}

	// Fail on a missing source rather than reporting a no-op success: the caller
	// (agent deletion) treats "source gone" as proof the move happened.
	if _, err := os.Lstat(from); err != nil {
		return err
	}

	return renameNoReplace(from, to)
}

func destinationExistsError(to string) error {
	return fmt.Errorf("refusing to rename over an existing path: %s", to)
}
