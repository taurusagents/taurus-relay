package fileops

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/taurusagents/taurus-relay/internal/protocol"
)

// Copy copies files host-side, on the node, through the ownership choke point.
func Copy(p *protocol.FileCopyPayload) (*protocol.FileCopyResultPayload, error) {
	return CopyContext(context.Background(), p)
}

// CopyContext backs the file.copy verb.
//
// Taurus uses it to mirror a delegated child's /taurus run artifacts into the
// parent agent's drive. That used to be a `sh -c 'mkdir -p … && cp -f …'` sent
// through proc.run, which bypassed internal/fileops entirely: the mirrored
// directories and files landed owned by the relay's own uid, and were readable
// by the container only because the relay's umask happened to leave them
// world-readable. Adding UMask=0077 to the relay unit — an ordinary hardening
// line — would silently have made a parent agent unable to read the images its
// child produced.
//
// The copy stays local to the node (no byte round-trip through the daemon) and
// every directory and file it creates goes through EnsureOwnedDir /
// chownCreatedFile like every other write.
func CopyContext(ctx context.Context, p *protocol.FileCopyPayload) (*protocol.FileCopyResultPayload, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if len(p.Pairs) == 0 {
		return &protocol.FileCopyResultPayload{Copied: 0}, nil
	}

	copied := 0
	for _, pair := range p.Pairs {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		src, err := ValidatePath(pair.Src)
		if err != nil {
			return nil, err
		}
		dest, err := ValidatePath(pair.Dest)
		if err != nil {
			return nil, err
		}
		if err := copyOneOwned(src, dest, os.FileMode(p.Mode)); err != nil {
			return nil, err
		}
		copied++
	}

	return &protocol.FileCopyResultPayload{Copied: copied}, nil
}

func copyOneOwned(src, dest string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("copy %s: not a regular file", src)
	}
	if mode == 0 {
		mode = info.Mode().Perm()
	}

	dir := filepath.Dir(dest)
	if err := EnsureOwnedDir(dir, 0); err != nil {
		return fmt.Errorf("copy %s: create directories: %w", dest, err)
	}

	// Same publish-by-rename shape as file.write: a reader of the destination
	// never sees a half-copied file, and ownership is applied to the staging file
	// so the published inode is already correct.
	tmp, tmpPath, err := createStagingFile(dir, filepath.Base(dest))
	if err != nil {
		return fmt.Errorf("copy %s: create temp file: %w", dest, err)
	}
	defer func() {
		_ = tmp.Close()
		if tmpPath != "" {
			_ = os.Remove(tmpPath)
		}
	}()

	// NOTE: io.Copy is not interruptible, so a very large artifact cannot be
	// cancelled mid-stream the way the killable `sh -c cp` it replaced could. The
	// caller checks the runtime context between pairs. Artifacts are images and
	// tool outputs (single-digit MB), so this is recorded rather than fixed; a
	// chunked copy with a per-chunk checkContext is the fix if that changes.
	if _, err := io.Copy(tmp, in); err != nil {
		return fmt.Errorf("copy %s -> %s: %w", src, dest, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("copy %s: chmod: %w", dest, err)
	}
	if err := chownCreatedFile(tmp, dest); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("copy %s: close: %w", dest, err)
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		return fmt.Errorf("copy %s -> %s: %w", src, dest, err)
	}
	tmpPath = ""
	return nil
}
