// taurus-relay connects a local machine to a Taurus daemon via WebSocket,
// enabling agents to execute commands and manage files remotely.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/taurusagents/taurus-relay/cmd"
	"github.com/taurusagents/taurus-relay/internal/config"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "connect":
		connectCmd := flag.NewFlagSet("connect", flag.ExitOnError)
		server := connectCmd.String("server", "", "Taurus daemon URL (e.g., https://taurus.example.com)")
		token := connectCmd.String("token", "", "One-time registration token")
		insecure := connectCmd.Bool("insecure", false, "Allow non-TLS (ws://) connections (for local development)")
		configDir := connectCmd.String("config-dir", "", "Directory containing config.json (default: ~/.config/taurus-relay, or $TAURUS_RELAY_CONFIG_DIR)")
		maxSessions := connectCmd.Int("max-sessions", cmd.MaxSessionsFlagUnset, "Max concurrent sessions, 0 = unlimited (default: 128, or $TAURUS_RELAY_MAX_SESSIONS)")
		connectCmd.Parse(os.Args[2:])
		config.SetDir(*configDir)

		maxSessionsValue, err := cmd.ValidatedMaxSessionsFlag(connectCmd, "max-sessions", *maxSessions)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

		if err := cmd.Connect(*server, *token, *insecure, maxSessionsValue); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "node":
		nodeCmd := flag.NewFlagSet("node", flag.ExitOnError)
		server := nodeCmd.String("server", "", "Taurus control plane URL (e.g., https://your-taurus-host.example)")
		name := nodeCmd.String("name", "", "Node name")
		host := nodeCmd.String("host", "", "Node public host/IP")
		token := nodeCmd.String("token", "", "Node enrollment token")
		dataPath := nodeCmd.String("data-path", "/data/taurus", "Node data root path")
		maxContainers := nodeCmd.Int("max-containers", 0, "Maximum containers allowed (0 = unlimited)")
		insecure := nodeCmd.Bool("insecure", false, "Allow non-TLS (ws://) connections (for local development)")
		maxSessions := nodeCmd.Int("max-sessions", cmd.MaxSessionsFlagUnset, "Max concurrent proc sessions, 0 = unlimited (default: 1048576, or $TAURUS_RELAY_MAX_SESSIONS)")
		driveOwner := nodeCmd.String("drive-owner", "", "Owner for drive dirs the relay creates: \"<uid>:<gid>\" docker userns-remap base, or \"none\" (required; or $TAURUS_DRIVE_OWNER)")
		nodeCmd.Parse(os.Args[2:])

		maxSessionsValue, err := cmd.ValidatedMaxSessionsFlag(nodeCmd, "max-sessions", *maxSessions)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

		if err := cmd.Node(*server, *name, *host, *token, *dataPath, *maxContainers, *insecure, maxSessionsValue, *driveOwner); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "status":
		statusCmd := flag.NewFlagSet("status", flag.ExitOnError)
		configDir := statusCmd.String("config-dir", "", "Directory containing config.json (default: ~/.config/taurus-relay, or $TAURUS_RELAY_CONFIG_DIR)")
		statusCmd.Parse(os.Args[2:])
		config.SetDir(*configDir)

		if err := cmd.Status(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "version":
		cmd.Version()

	case "help", "-h", "--help":
		printUsage()

	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Print(`Usage: taurus-relay <command> [options]

Commands:
  connect    Connect to a Taurus daemon as a user relay
  node       Connect to a Taurus daemon as a container node relay (Linux hosts only)
  status     Show relay status and saved credentials
  version    Print version information
  help       Show this help

Connect options:
  --server <url>    Taurus daemon URL (e.g., https://taurus.example.com)
  --token <token>   One-time registration token (required for first connection)
  --insecure        Allow non-TLS (ws://) connections (for local development)
  --config-dir <dir>  Directory containing config.json (default: ~/.config/taurus-relay,
                      or the TAURUS_RELAY_CONFIG_DIR environment variable)
  --max-sessions <n>  Max concurrent sessions, 0 = unlimited (default: 128,
                      or the TAURUS_RELAY_MAX_SESSIONS environment variable)

Node options:
  --server <url>           Taurus control plane URL (required)
  --name <name>            Node name (required)
  --host <host>            Node public host/IP (required)
  --token <token>          Enrollment token (required)
  --data-path <path>       Data root (default: /data/taurus)
  --max-containers <n>     Container cap (default: 0 = unlimited)
  --max-sessions <n>       Max concurrent proc sessions, 0 = unlimited (default: 1048576,
                           or the TAURUS_RELAY_MAX_SESSIONS environment variable)
  --drive-owner <spec>     REQUIRED. Owner for the agent drive directories the relay
                           creates: "<uid>:<gid>" — the dockerd userns-remap base,
                           normally 100000:100000 — or "none" on nodes that do not run
                           dockerd with userns-remap. May also be set via the
                           TAURUS_DRIVE_OWNER environment variable. Node mode refuses to
                           start when it is unset, or when the relay cannot actually
                           apply that ownership (the systemd unit needs CAP_CHOWN,
                           CAP_DAC_OVERRIDE and CAP_FOWNER).
  --insecure               Allow non-TLS (ws://) connections

  Note: Windows releases support connect mode only; node mode is unsupported on Windows.

Status options:
  --config-dir <dir>  Directory containing config.json (same as connect)

Examples:
  taurus-relay connect --token abc123 --server https://taurus.example.com
  taurus-relay connect --server https://taurus.example.com  # uses saved credentials
  taurus-relay connect --insecure --server http://localhost:3000  # local dev
  taurus-relay connect --config-dir ~/relay-work --server https://taurus.example.com  # separate identity
  taurus-relay node --server https://your-taurus-host.example --name hetzner-1 --host 203.0.113.10 --token <node-enrollment-token> --drive-owner 100000:100000
  taurus-relay status
`)
}
