package cmd

import (
	"flag"
	"fmt"
	"os"
	"strconv"
)

// EnvMaxSessions overrides the concurrent-session cap when the --max-sessions
// flag is not provided, mirroring the TAURUS_RELAY_CONFIG_DIR precedent.
const EnvMaxSessions = "TAURUS_RELAY_MAX_SESSIONS"

// resolveMaxSessions applies the precedence flag > env > mode default and
// returns the effective session cap. flagValue < 0 means the flag was not
// provided; an explicit 0 (from flag or env) means unlimited.
func resolveMaxSessions(flagValue int, envValue string, modeDefault int) (int, error) {
	if flagValue >= 0 {
		return flagValue, nil
	}
	if envValue != "" {
		n, err := strconv.Atoi(envValue)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid %s value %q: must be a non-negative integer (0 = unlimited)", EnvMaxSessions, envValue)
		}
		return n, nil
	}
	return modeDefault, nil
}

func resolveMaxSessionsFromEnv(flagValue, modeDefault int) (int, error) {
	return resolveMaxSessions(flagValue, os.Getenv(EnvMaxSessions), modeDefault)
}

// MaxSessionsFlagUnset is the sentinel meaning "--max-sessions was not
// provided", letting the env/default precedence chain take over.
const MaxSessionsFlagUnset = -1

// ValidatedMaxSessionsFlag inspects the parsed flag set to distinguish a flag
// that was never provided (returns MaxSessionsFlagUnset) from an explicit
// value. Explicit negative values — including -1, which would otherwise be
// mistaken for the unset sentinel — are rejected instead of silently falling
// through to the env/default precedence chain.
func ValidatedMaxSessionsFlag(fs *flag.FlagSet, name string, value int) (int, error) {
	provided := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			provided = true
		}
	})
	if !provided {
		return MaxSessionsFlagUnset, nil
	}
	if value < 0 {
		return 0, fmt.Errorf("invalid --%s value %d: must be a non-negative integer (0 = unlimited)", name, value)
	}
	return value, nil
}
