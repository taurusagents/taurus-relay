// taurus-relay connects a local machine to a Taurus daemon via WebSocket,
// enabling agents to execute commands and manage files remotely.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/taurusagents/taurus-relay/cmd"
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
		connectCmd.Parse(os.Args[2:])

		if err := cmd.Connect(*server, *token, *insecure); err != nil {
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
		nodeCmd.Parse(os.Args[2:])

		if err := cmd.Node(*server, *name, *host, *token, *dataPath, *maxContainers, *insecure); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "status":
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
  node       Connect to a Taurus daemon as a container node relay
  status     Show relay status and saved credentials
  version    Print version information
  help       Show this help

Connect options:
  --server <url>    Taurus daemon URL (e.g., https://taurus.example.com)
  --token <token>   One-time registration token (required for first connection)
  --insecure        Allow non-TLS (ws://) connections (for local development)

Node options:
  --server <url>           Taurus control plane URL (required)
  --name <name>            Node name (required)
  --host <host>            Node public host/IP (required)
  --token <token>          Enrollment token (required)
  --data-path <path>       Data root (default: /data/taurus)
  --max-containers <n>     Container cap (default: 0 = unlimited)
  --insecure               Allow non-TLS (ws://) connections

Examples:
  taurus-relay connect --token abc123 --server https://taurus.example.com
  taurus-relay connect --server https://taurus.example.com  # uses saved credentials
  taurus-relay connect --insecure --server http://localhost:3000  # local dev
  taurus-relay node --server https://your-taurus-host.example --name hetzner-1 --host 203.0.113.10 --token <node-enrollment-token>
  taurus-relay status
`)
}
