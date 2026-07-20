package tunnel

import "strings"

// redactEnvValuesInArgv returns a copy of argv that is safe to log. Spawn argv
// in node mode is typically `docker exec ... -e KEY=VALUE ... <cmd>` and those
// env values routinely contain credentials, so the value of every environment
// assignment passed via -e/--env is replaced with [REDACTED]. Keys and the
// overall argv structure are preserved; unrelated tokens are never modified.
func redactEnvValuesInArgv(argv []string) []string {
	if len(argv) == 0 {
		return argv
	}
	out := make([]string, len(argv))
	copy(out, argv)
	for i := 0; i < len(out); i++ {
		tok := out[i]
		switch {
		case tok == "-e" || tok == "--env":
			// Separate-token form: the assignment is the next token.
			if i+1 < len(out) {
				out[i+1] = redactEnvAssignment(out[i+1])
				i++
			}
		case strings.HasPrefix(tok, "--env="):
			out[i] = "--env=" + redactEnvAssignment(tok[len("--env="):])
		case strings.HasPrefix(tok, "-e="):
			out[i] = "-e=" + redactEnvAssignment(tok[len("-e="):])
		}
	}
	return out
}

// redactEnvAssignment turns KEY=VALUE into KEY=[REDACTED], splitting at the
// first '='. A token without '=' is the docker "inherit KEY from the CLI
// environment" form: it carries no value, so it is returned unchanged.
func redactEnvAssignment(tok string) string {
	key, _, found := strings.Cut(tok, "=")
	if !found {
		return tok
	}
	return key + "=[REDACTED]"
}
