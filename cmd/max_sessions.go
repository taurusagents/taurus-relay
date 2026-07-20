package cmd

import (
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
