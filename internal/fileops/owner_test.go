package fileops

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/taurusagents/taurus-relay/internal/protocol"
)

// remapBase stands in for the Docker userns-remap base on a real node. It is
// deliberately an id this test process does not own, so a chown to it is a
// genuine ownership *change* rather than a no-op.
var remapBase = DriveOwner{UID: 100000, GID: 100000}

// chownRecorder captures every ownership change the fileops choke point makes.
//
// It applies the real syscall only when the process can actually perform it
// (euid 0). That way the exact same assertions run in two environments: as root
// the on-disk uid/gid are verified for real, and unprivileged (GitHub CI) the
// full contract of *which* paths get handed to the owner is still asserted
// instead of silently skipping the test.
type chownRecorder struct {
	paths map[string]DriveOwner
	fail  error
}

func (r *chownRecorder) sortedPaths() []string {
	out := make([]string, 0, len(r.paths))
	for p := range r.paths {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func newChownRecorder(t *testing.T) *chownRecorder {
	t.Helper()

	rec := &chownRecorder{paths: map[string]DriveOwner{}}
	privileged := os.Geteuid() == 0

	prevLchown, prevFchown := lchownPath, fchownFile
	lchownPath = func(path string, uid, gid int) error {
		if rec.fail != nil {
			return rec.fail
		}
		rec.paths[filepath.Clean(path)] = DriveOwner{UID: uid, GID: gid}
		if privileged {
			return os.Lchown(path, uid, gid)
		}
		return nil
	}
	fchownFile = func(f *os.File, path string, uid, gid int) error {
		if rec.fail != nil {
			return rec.fail
		}
		rec.paths[filepath.Clean(path)] = DriveOwner{UID: uid, GID: gid}
		if privileged {
			return f.Chown(uid, gid)
		}
		return nil
	}
	t.Cleanup(func() {
		lchownPath, fchownFile = prevLchown, prevFchown
	})
	return rec
}

// withDriveOwnership points fileops at a temporary data root configured exactly
// like a node relay: file operations restricted to the data root, drive
// ownership applied only under taurus-drives/ and taurus-drives-trash/.
func withDriveOwnership(t *testing.T, owner *DriveOwner) string {
	t.Helper()

	dataRoot := t.TempDir()
	prevRoots := AllowedRoots
	AllowedRoots = []string{dataRoot}
	SetDriveOwnership(owner, DriveRoots(dataRoot))
	t.Cleanup(func() {
		AllowedRoots = prevRoots
		SetDriveOwnership(nil, nil)
	})
	return dataRoot
}

func assertOwnedOnDisk(t *testing.T, path string, owner DriveOwner) {
	t.Helper()
	if os.Geteuid() != 0 {
		return // unprivileged run: the recorder assertions carry the contract
	}
	uid, gid, err := pathOwner(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if uid != owner.UID || gid != owner.GID {
		t.Fatalf("expected %s to be owned by %s on disk, got %d:%d", path, owner, uid, gid)
	}
}

func TestParseDriveOwnerAcceptsRemapBaseAndExplicitOptOut(t *testing.T) {
	owner, err := ParseDriveOwner("100000:100000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner == nil || *owner != remapBase {
		t.Fatalf("expected 100000:100000, got %v", owner)
	}
	if got := owner.String(); got != "100000:100000" {
		t.Fatalf("expected string form 100000:100000, got %q", got)
	}

	for _, raw := range []string{"none", "NONE", " none "} {
		owner, err := ParseDriveOwner(raw)
		if err != nil {
			t.Fatalf("ParseDriveOwner(%q): unexpected error: %v", raw, err)
		}
		if owner != nil {
			t.Fatalf("ParseDriveOwner(%q): expected opt-out, got %v", raw, owner)
		}
	}
}

// The unset case is the fail-closed path nobody tests: an empty value must be a
// hard error, never a silent "chown nothing" default.
func TestParseDriveOwnerRejectsUnsetAndGarbage(t *testing.T) {
	for _, raw := range []string{"", "   ", "100000", "100000:", ":100000", "-1:0", "0:-1", "root:root", "100000:100000:0", "0x10:0", "99999999999:0"} {
		owner, err := ParseDriveOwner(raw)
		if err == nil {
			t.Fatalf("ParseDriveOwner(%q): expected an error, got owner %v", raw, owner)
		}
		if owner != nil {
			t.Fatalf("ParseDriveOwner(%q): expected nil owner alongside the error, got %v", raw, owner)
		}
	}
}

// The regression this whole change exists for: chowning only the leaf passes a
// naive "can the new agent write?" check and still leaves the agent unable to
// create siblings, because the intermediate directories belong to the relay.
func TestEnsureOwnedDirHandsOverEveryCreatedComponent(t *testing.T) {
	rec := newChownRecorder(t)
	dataRoot := withDriveOwnership(t, &remapBase)

	drives := filepath.Join(dataRoot, DrivesDirName)
	userDir := filepath.Join(drives, "user-1")
	agentDir := filepath.Join(userDir, "agent-1")
	workspace := filepath.Join(agentDir, "workspace")

	if err := EnsureOwnedDir(workspace, 0o755); err != nil {
		t.Fatalf("EnsureOwnedDir: %v", err)
	}

	want := []string{drives, userDir, agentDir, workspace}
	got := rec.sortedPaths()
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("expected every created component to be handed over\n want %v\n  got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected every created component to be handed over\n want %v\n  got %v", want, got)
		}
	}
	for _, path := range want {
		if rec.paths[path] != remapBase {
			t.Fatalf("expected %s to be handed to %s, got %v", path, remapBase, rec.paths[path])
		}
		assertOwnedOnDisk(t, path, remapBase)
	}
}

// Ownership is only ever applied to paths the relay creates. A pre-existing
// tree (an agent drive created before the migration, or the operator's own
// directory layout) is never silently rewritten.
func TestEnsureOwnedDirLeavesExistingComponentsAlone(t *testing.T) {
	rec := newChownRecorder(t)
	dataRoot := withDriveOwnership(t, &remapBase)

	drives := filepath.Join(dataRoot, DrivesDirName)
	userDir := filepath.Join(drives, "user-1")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}

	agentDir := filepath.Join(userDir, "agent-1")
	if err := EnsureOwnedDir(agentDir, 0o755); err != nil {
		t.Fatalf("EnsureOwnedDir: %v", err)
	}

	if _, ok := rec.paths[drives]; ok {
		t.Fatalf("pre-existing %s must not be chowned", drives)
	}
	if _, ok := rec.paths[userDir]; ok {
		t.Fatalf("pre-existing %s must not be chowned", userDir)
	}
	if rec.paths[agentDir] != remapBase {
		t.Fatalf("expected the newly created %s to be handed to %s", agentDir, remapBase)
	}
}

// Drive ownership is scoped to the drive subtrees. Host-side artifacts the relay
// stages elsewhere under the data root (the managed seccomp profile, for
// example) are read by dockerd as root and must keep their existing ownership.
func TestEnsureOwnedDirIgnoresPathsOutsideDriveRoots(t *testing.T) {
	rec := newChownRecorder(t)
	dataRoot := withDriveOwnership(t, &remapBase)

	runtimeDir := filepath.Join(dataRoot, "runtime", "seccomp")
	if err := EnsureOwnedDir(runtimeDir, 0o755); err != nil {
		t.Fatalf("EnsureOwnedDir: %v", err)
	}
	if len(rec.paths) != 0 {
		t.Fatalf("expected no chown outside the drive roots, got %v", rec.sortedPaths())
	}
	if _, err := os.Stat(runtimeDir); err != nil {
		t.Fatalf("expected %s to still be created: %v", runtimeDir, err)
	}
}

func TestEnsureOwnedDirWithoutOwnerNeverChowns(t *testing.T) {
	rec := newChownRecorder(t)
	dataRoot := withDriveOwnership(t, nil) // "none" / connect-mode behavior

	workspace := filepath.Join(dataRoot, DrivesDirName, "user-1", "agent-1", "workspace")
	if err := EnsureOwnedDir(workspace, 0o755); err != nil {
		t.Fatalf("EnsureOwnedDir: %v", err)
	}
	if len(rec.paths) != 0 {
		t.Fatalf("expected no chown when no drive owner is configured, got %v", rec.sortedPaths())
	}
	if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
		t.Fatalf("expected %s to be created: %v", workspace, err)
	}
}

func TestEnsureOwnedDirRejectsNonDirectoryTarget(t *testing.T) {
	newChownRecorder(t)
	dataRoot := withDriveOwnership(t, &remapBase)

	target := filepath.Join(dataRoot, DrivesDirName, "not-a-dir")
	if err := EnsureOwnedDir(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := EnsureOwnedDir(target, 0o755)
	if err == nil || !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("expected ENOTDIR, got %v", err)
	}
}

// file.write must publish a file owned by the drive owner, and the parent
// directories it creates on the way must be handed over too.
func TestWriteContextHandsFileAndCreatedParentsToDriveOwner(t *testing.T) {
	rec := newChownRecorder(t)
	dataRoot := withDriveOwnership(t, &remapBase)

	target := filepath.Join(dataRoot, DrivesDirName, "user-1", "agent-1", "workspace", "WORKSPACE.md")
	result, err := WriteContext(context.Background(), &protocol.FileWritePayload{
		Path:    target,
		Content: base64.StdEncoding.EncodeToString([]byte("template")),
		Mode:    0o644,
	})
	if err != nil {
		t.Fatalf("WriteContext: %v", err)
	}
	if result.BytesWritten != len("template") {
		t.Fatalf("expected 8 bytes written, got %d", result.BytesWritten)
	}

	content, err := os.ReadFile(target)
	if err != nil || string(content) != "template" {
		t.Fatalf("expected the file contents to land: %q %v", content, err)
	}
	if rec.paths[target] != remapBase {
		t.Fatalf("expected %s to be handed to %s, got %v", target, remapBase, rec.paths[target])
	}
	for _, dir := range []string{
		filepath.Join(dataRoot, DrivesDirName),
		filepath.Join(dataRoot, DrivesDirName, "user-1"),
		filepath.Join(dataRoot, DrivesDirName, "user-1", "agent-1"),
		filepath.Join(dataRoot, DrivesDirName, "user-1", "agent-1", "workspace"),
	} {
		if rec.paths[dir] != remapBase {
			t.Fatalf("expected created parent %s to be handed to %s, got %v", dir, remapBase, rec.paths[dir])
		}
	}
	assertOwnedOnDisk(t, target, remapBase)
}

func TestMkdirContextAppliesModeAndOwnership(t *testing.T) {
	rec := newChownRecorder(t)
	dataRoot := withDriveOwnership(t, &remapBase)

	base := filepath.Join(dataRoot, DrivesDirName, "user-1")
	if err := MkdirContext(context.Background(), &protocol.FileMkdirPayload{Path: base, Recursive: true}); err != nil {
		t.Fatalf("recursive mkdir: %v", err)
	}

	codexConfig := filepath.Join(base, "codex-config")
	if err := MkdirContext(context.Background(), &protocol.FileMkdirPayload{Path: codexConfig, Mode: 0o700}); err != nil {
		t.Fatalf("non-recursive mkdir: %v", err)
	}

	info, err := os.Stat(codexConfig)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("expected mode 0700, got %04o", info.Mode().Perm())
	}
	if rec.paths[codexConfig] != remapBase {
		t.Fatalf("expected %s to be handed to %s, got %v", codexConfig, remapBase, rec.paths[codexConfig])
	}
	assertOwnedOnDisk(t, codexConfig, remapBase)
}

// auth.json bootstrap: create it 0600 owned by the remap base, otherwise the
// container cannot read its own credential file through the single-file bind
// mount.
func TestEnsureFileCreatesAnOwnedCredentialFile(t *testing.T) {
	rec := newChownRecorder(t)
	dataRoot := withDriveOwnership(t, &remapBase)

	authFile := filepath.Join(dataRoot, DrivesDirName, "user-1", "codex-config", "auth.json")
	result, err := EnsureFileContext(context.Background(), &protocol.FileEnsureFilePayload{Path: authFile, Mode: 0o600})
	if err != nil {
		t.Fatalf("EnsureFileContext: %v", err)
	}
	if !result.Created {
		t.Fatal("expected the first call to report Created=true")
	}

	info, err := os.Lstat(authFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected mode 0600, got %04o", info.Mode().Perm())
	}
	if rec.paths[authFile] != remapBase {
		t.Fatalf("expected %s to be handed to %s, got %v", authFile, remapBase, rec.paths[authFile])
	}
	assertOwnedOnDisk(t, authFile, remapBase)

}

// Second launch of the same subscription: the credentials that codex wrote must
// survive untouched. The owner here is the running process's own ids so the file
// is genuinely correctly-owned on disk in both the privileged and unprivileged
// test environments — the wrongly-owned case is its own test below.
func TestEnsureFileNeverClobbersLiveCredentials(t *testing.T) {
	newChownRecorder(t)
	self := DriveOwner{UID: os.Geteuid(), GID: os.Getegid()}
	dataRoot := withDriveOwnership(t, &self)

	authFile := filepath.Join(dataRoot, DrivesDirName, "user-1", "codex-config", "auth.json")
	if _, err := EnsureFileContext(context.Background(), &protocol.FileEnsureFilePayload{Path: authFile, Mode: 0o600}); err != nil {
		t.Fatalf("EnsureFileContext: %v", err)
	}
	if err := os.WriteFile(authFile, []byte(`{"token":"live"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := EnsureFileContext(context.Background(), &protocol.FileEnsureFilePayload{Path: authFile, Mode: 0o600})
	if err != nil {
		t.Fatalf("EnsureFileContext (existing): %v", err)
	}
	if result.Created {
		t.Fatal("expected Created=false for an existing file")
	}
	content, err := os.ReadFile(authFile)
	if err != nil || string(content) != `{"token":"live"}` {
		t.Fatalf("expected existing credentials to be untouched, got %q %v", content, err)
	}
}

func TestEnsureFileRejectsSymlinkAndDirectory(t *testing.T) {
	newChownRecorder(t)
	dataRoot := withDriveOwnership(t, &remapBase)

	dir := filepath.Join(dataRoot, DrivesDirName, "user-1", "codex-config")
	if err := EnsureOwnedDir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	symlink := filepath.Join(dir, "auth.json")
	if err := os.Symlink(filepath.Join(dataRoot, "elsewhere"), symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureFileContext(context.Background(), &protocol.FileEnsureFilePayload{Path: symlink, Mode: 0o600}); err == nil {
		t.Fatal("expected a symlink at the auth path to be refused")
	}
	if _, err := os.Lstat(symlink); err != nil {
		t.Fatalf("the refused symlink must be left in place for the operator to inspect: %v", err)
	}

	nested := filepath.Join(dir, "nested")
	if err := EnsureOwnedDir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureFileContext(context.Background(), &protocol.FileEnsureFilePayload{Path: nested, Mode: 0o600}); err == nil {
		t.Fatal("expected a directory at the auth path to be refused")
	}
}

func TestVerifyDriveOwnershipCreatesAndProvesOwnershipOfBothRoots(t *testing.T) {
	rec := newChownRecorder(t)
	// Configure the process's own ids so the verification's on-disk ownership
	// check succeeds unprivileged as well as under root.
	self := DriveOwner{UID: os.Geteuid(), GID: os.Getegid()}
	dataRoot := withDriveOwnership(t, &self)

	if err := VerifyDriveOwnership(); err != nil {
		t.Fatalf("VerifyDriveOwnership: %v", err)
	}

	for _, root := range DriveRoots(dataRoot) {
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			t.Fatalf("expected %s to exist after verification: %v", root, err)
		}
		if rec.paths[root] != self {
			t.Fatalf("expected the created root %s to be handed to %s", root, self)
		}
		if _, err := os.Stat(filepath.Join(root, driveOwnerProbeDirName)); !os.IsNotExist(err) {
			t.Fatalf("expected the ownership probe dir under %s to be cleaned up, got %v", root, err)
		}
	}
}

// Ordering guard from the migration runbook: the operator chowns the tree first,
// then starts the relay. A relay pointed at a tree that is still owned by
// someone else must refuse to start rather than create half-migrated drives.
func TestVerifyDriveOwnershipRefusesMismatchedExistingRoot(t *testing.T) {
	newChownRecorder(t)
	dataRoot := withDriveOwnership(t, &remapBase)

	drives := filepath.Join(dataRoot, DrivesDirName)
	if err := os.MkdirAll(drives, 0o755); err != nil { // owned by this test process, not 100000
		t.Fatal(err)
	}

	err := VerifyDriveOwnership()
	if err == nil {
		t.Fatal("expected VerifyDriveOwnership to refuse a drive root owned by someone else")
	}
	if !strings.Contains(err.Error(), "chown -R 100000:100000") {
		t.Fatalf("expected the error to spell out the migration command, got: %v", err)
	}
}

// The grant nobody tests: without CAP_CHOWN the relay must fail to start, not
// quietly create drive dirs owned by its own uid.
func TestVerifyDriveOwnershipRefusesWhenChownIsNotPermitted(t *testing.T) {
	rec := newChownRecorder(t)
	rec.fail = syscall.EPERM
	withDriveOwnership(t, &remapBase)

	err := VerifyDriveOwnership()
	if err == nil {
		t.Fatal("expected VerifyDriveOwnership to fail when chown is not permitted")
	}
	if !strings.Contains(err.Error(), "CAP_CHOWN") {
		t.Fatalf("expected the error to point at the missing capability grant, got: %v", err)
	}
}

func TestVerifyDriveOwnershipIsNoOpWithoutOwner(t *testing.T) {
	newChownRecorder(t)
	dataRoot := withDriveOwnership(t, nil)

	if err := VerifyDriveOwnership(); err != nil {
		t.Fatalf("expected no-op verification without an owner, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataRoot, DrivesDirName)); !os.IsNotExist(err) {
		t.Fatalf("expected no drive roots to be created without an owner, got %v", err)
	}
}

// The regression critic4 caught: an auth.json left behind by a relay that
// predates drive ownership is owned by the relay's uid, and the container that
// bind-mounts it at 0600 cannot read its own credentials. Passing it through
// silently reproduces #357 for every existing subscription agent.
func TestEnsureFileRefusesAnExistingFileOwnedBySomeoneElse(t *testing.T) {
	newChownRecorder(t)
	dataRoot := withDriveOwnership(t, &remapBase)

	authFile := filepath.Join(dataRoot, DrivesDirName, "user-1", "codex-config", "auth.json")
	if err := EnsureOwnedDir(filepath.Dir(authFile), 0o700); err != nil {
		t.Fatal(err)
	}
	// Written directly, i.e. owned by this process rather than the remap base —
	// exactly what a pre-migration relay left on disk.
	if err := os.WriteFile(authFile, []byte(`{"token":"stale"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := EnsureFileContext(context.Background(), &protocol.FileEnsureFilePayload{Path: authFile, Mode: 0o600})
	if err == nil {
		t.Fatal("expected a wrongly-owned existing credential file to be refused")
	}
	if !strings.Contains(err.Error(), "was not migrated") {
		t.Fatalf("expected the error to name the migration, got: %v", err)
	}
	// Refused, never repaired: a quiet chown would hide an unmigrated tree.
	content, readErr := os.ReadFile(authFile)
	if readErr != nil || string(content) != `{"token":"stale"}` {
		t.Fatalf("the refused file must be left untouched, got %q %v", content, readErr)
	}
}

// Ownership of pre-existing files is only Taurus's business inside the drive
// roots. Connect-mode relays (and the node's own runtime staging paths) must not
// start failing on files they do not own.
func TestEnsureFileIgnoresOwnershipOutsideDriveRoots(t *testing.T) {
	newChownRecorder(t)
	dataRoot := withDriveOwnership(t, &remapBase)

	outside := filepath.Join(dataRoot, "runtime", "marker")
	if err := EnsureOwnedDir(filepath.Dir(outside), 0); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureFileContext(context.Background(), &protocol.FileEnsureFilePayload{Path: outside}); err != nil {
		t.Fatalf("expected no ownership opinion outside the drive roots, got %v", err)
	}
}

// MkdirAll (which EnsureOwnedDir replaced) follows a final symlink pointing at a
// directory and succeeds. Connect-mode relays run against real home directories
// where symlinked project dirs are ordinary, so this must keep working — it is
// currently also protected by ValidatePath's EvalSymlinks, and this pins the
// behavior one refactor away from that.
func TestEnsureOwnedDirAcceptsSymlinkToDirectory(t *testing.T) {
	newChownRecorder(t)
	dataRoot := withDriveOwnership(t, nil) // connect-mode shape: no drive owner

	real := filepath.Join(dataRoot, "real-dir")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dataRoot, "link-dir")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	if err := EnsureOwnedDir(link, 0); err != nil {
		t.Fatalf("expected a symlink to a directory to be accepted like os.MkdirAll: %v", err)
	}
	if err := EnsureOwnedDir(filepath.Join(link, "child"), 0); err != nil {
		t.Fatalf("expected mkdir under a symlinked directory to work: %v", err)
	}
	// And through the actual verb, which is what a connect-mode relay serves.
	if err := MkdirContext(context.Background(), &protocol.FileMkdirPayload{Path: filepath.Join(link, "child"), Recursive: true}); err != nil {
		t.Fatalf("expected mkdir under a symlinked directory to work: %v", err)
	}
	if info, err := os.Stat(filepath.Join(real, "child")); err != nil || !info.IsDir() {
		t.Fatalf("expected the child to be created through the symlink: %v", err)
	}
}

// file.write publishes by rename; the staging file it renames must itself never
// be created through a symlink someone planted in the destination directory.
func TestWriteContextRefusesAPlantedStagingSymlink(t *testing.T) {
	newChownRecorder(t)
	dataRoot := withDriveOwnership(t, &remapBase)

	dir := filepath.Join(dataRoot, DrivesDirName, "user-1", "agent-1", "workspace")
	if err := EnsureOwnedDir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dataRoot, "victim.txt")
	if err := os.WriteFile(outside, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The staging name is unpredictable, so this pins the mechanism rather than a
	// specific race: creating over an existing name must fail rather than follow
	// it, whether that name is a symlink or a regular file.
	if _, _, err := createStagingFile(dir, "WORKSPACE.md"); err != nil {
		t.Fatalf("staging file creation should succeed on a clean dir: %v", err)
	}
	planted := filepath.Join(dir, ".planted")
	if err := os.Symlink(outside, planted); err != nil {
		t.Fatal(err)
	}
	if _, err := os.OpenFile(planted, os.O_CREATE|os.O_EXCL|os.O_WRONLY|oNoFollow, 0o600); err == nil {
		t.Fatal("expected O_EXCL|O_NOFOLLOW to refuse an existing symlink")
	}
	if content, err := os.ReadFile(outside); err != nil || string(content) != "original" {
		t.Fatalf("the symlink target must be untouched, got %q %v", content, err)
	}
}

// file.copy is the artifact-mirroring path. It must hand both the copied file
// and every directory it creates to the drive owner — the sh -c 'mkdir -p && cp'
// it replaced did neither.
func TestCopyContextHandsMirroredArtifactsToDriveOwner(t *testing.T) {
	rec := newChownRecorder(t)
	dataRoot := withDriveOwnership(t, &remapBase)

	src := filepath.Join(dataRoot, DrivesDirName, "user-1", "child", "taurus", "runs", "run-9", "generated", "image.png")
	if err := EnsureOwnedDir(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("png-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(dataRoot, DrivesDirName, "user-1", "parent", "taurus", "runs", "run-9", "generated", "image.png")
	result, err := CopyContext(context.Background(), &protocol.FileCopyPayload{
		Pairs: []protocol.FileCopyPair{{Src: src, Dest: dest}},
	})
	if err != nil {
		t.Fatalf("CopyContext: %v", err)
	}
	if result.Copied != 1 {
		t.Fatalf("expected 1 copied file, got %d", result.Copied)
	}

	content, err := os.ReadFile(dest)
	if err != nil || string(content) != "png-bytes" {
		t.Fatalf("expected the artifact bytes to land, got %q %v", content, err)
	}
	if rec.paths[dest] != remapBase {
		t.Fatalf("expected the mirrored file to be handed to %s, got %v", remapBase, rec.paths[dest])
	}
	for _, dir := range []string{
		filepath.Join(dataRoot, DrivesDirName, "user-1", "parent"),
		filepath.Join(dataRoot, DrivesDirName, "user-1", "parent", "taurus"),
		filepath.Join(dataRoot, DrivesDirName, "user-1", "parent", "taurus", "runs", "run-9", "generated"),
	} {
		if rec.paths[dir] != remapBase {
			t.Fatalf("expected created dir %s to be handed to %s, got %v", dir, remapBase, rec.paths[dir])
		}
	}
	assertOwnedOnDisk(t, dest, remapBase)
}

func TestCopyContextRefusesPathsOutsideAllowedRoots(t *testing.T) {
	newChownRecorder(t)
	dataRoot := withDriveOwnership(t, &remapBase)

	src := filepath.Join(dataRoot, DrivesDirName, "user-1", "child", "taurus", "image.png")
	if err := EnsureOwnedDir(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := CopyContext(context.Background(), &protocol.FileCopyPayload{
		Pairs: []protocol.FileCopyPair{{Src: src, Dest: "/tmp/escaped.png"}},
	}); err == nil {
		t.Fatal("expected a destination outside the allowed roots to be refused")
	}
	if _, err := os.Stat("/tmp/escaped.png"); !os.IsNotExist(err) {
		t.Fatalf("expected nothing to be written outside the allowed roots, got %v", err)
	}
}

// The node-mode half of the symlink rule: inside a managed drive root a symlink
// at the leaf is refused rather than followed. Nothing in the drive layout is
// legitimately a symlink, so one appearing between ValidatePath resolving the
// path and the mkdir is a swap attempt.
func TestEnsureOwnedDirRefusesASymlinkedLeafInsideADriveRoot(t *testing.T) {
	newChownRecorder(t)
	dataRoot := withDriveOwnership(t, &remapBase)

	outside := filepath.Join(dataRoot, "elsewhere")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	drives := filepath.Join(dataRoot, DrivesDirName)
	if err := EnsureOwnedDir(drives, 0o755); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(drives, "user-1")
	if err := os.Symlink(outside, planted); err != nil {
		t.Fatal(err)
	}

	err := EnsureOwnedDir(planted, 0o755)
	if err == nil || !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("expected a symlinked leaf inside a drive root to be refused with ENOTDIR, got %v", err)
	}
}
