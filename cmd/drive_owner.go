package cmd

import (
	"fmt"
	"os"
	"runtime"

	"github.com/taurusagents/taurus-relay/internal/fileops"
)

// EnvDriveOwner names the environment variable that configures the userns-remap
// drive owner when --drive-owner is not passed, mirroring the
// TAURUS_RELAY_MAX_SESSIONS precedent.
const EnvDriveOwner = "TAURUS_DRIVE_OWNER"

// resolveDriveOwner applies the precedence flag > env and parses the result.
//
// There is deliberately **no default**. An unset value is a hard startup error
// in node mode: on a userns-remapped node it would silently produce drive
// directories owned by the relay's own uid, which the agent container cannot
// write — the quiet failure mode that made brand-new agents unlaunchable on
// staging. Operators of non-remapped nodes opt out explicitly with "none".
func resolveDriveOwner(flagValue, envValue string) (*fileops.DriveOwner, error) {
	raw := flagValue
	source := "--drive-owner"
	if raw == "" {
		raw = envValue
		source = EnvDriveOwner
	}

	owner, err := fileops.ParseDriveOwner(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	return owner, nil
}

func resolveDriveOwnerFromEnv(flagValue string) (*fileops.DriveOwner, error) {
	return resolveDriveOwner(flagValue, os.Getenv(EnvDriveOwner))
}

// validateDriveOwnerPlatform refuses a drive owner on platforms where unix
// ownership cannot be applied. Node mode already requires a non-Windows host;
// this keeps the failure explicit rather than surfacing as a chown error later.
func validateDriveOwnerPlatform(owner *fileops.DriveOwner, goos string) error {
	if owner == nil || goos == "linux" {
		return nil
	}
	return fmt.Errorf(
		"drive owner %s is configured but Docker userns-remap drive ownership is only supported on Linux (this host is %s); use --drive-owner %s",
		owner, goos, fileops.DriveOwnerNone)
}

// configureDriveOwnership parses, validates and installs the drive-ownership
// configuration for node mode, then proves on the real filesystem that the relay
// can actually apply it.
func configureDriveOwnership(flagValue, dataPath string) error {
	owner, err := resolveDriveOwnerFromEnv(flagValue)
	if err != nil {
		return err
	}
	if err := validateDriveOwnerPlatform(owner, runtime.GOOS); err != nil {
		return err
	}

	fileops.SetDriveOwnership(owner, fileops.DriveRoots(dataPath))
	return fileops.VerifyDriveOwnership()
}
