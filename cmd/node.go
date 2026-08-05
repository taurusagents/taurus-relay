package cmd

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/taurusagents/taurus-relay/internal/fileops"
	"github.com/taurusagents/taurus-relay/internal/health"
	"github.com/taurusagents/taurus-relay/internal/tunnel"
)

// Node handles the `taurus-relay node` command. maxSessionsFlag is the raw
// --max-sessions value (< 0 = flag not provided). driveOwnerFlag is the raw
// --drive-owner value ("" = not provided, falls back to TAURUS_DRIVE_OWNER).
func Node(server, name, host, token, dataPath string, maxContainers int, insecure bool, maxSessionsFlag int, driveOwnerFlag string) error {
	if err := validateNodePlatform(runtime.GOOS); err != nil {
		return err
	}
	maxSessions, err := resolveMaxSessionsFromEnv(maxSessionsFlag, tunnel.DefaultNodeMaxSessions)
	if err != nil {
		return err
	}
	if server == "" {
		return fmt.Errorf("--server is required")
	}
	if name == "" {
		return fmt.Errorf("--name is required")
	}
	if host == "" {
		return fmt.Errorf("--host is required")
	}
	if token == "" {
		return fmt.Errorf("--token is required")
	}
	if dataPath == "" {
		return fmt.Errorf("--data-path is required")
	}

	if !insecure && (strings.HasPrefix(server, "http://") || strings.HasPrefix(server, "ws://")) {
		return fmt.Errorf("non-TLS server URL %q is not allowed without --insecure flag", server)
	}
	if insecure && (strings.HasPrefix(server, "http://") || strings.HasPrefix(server, "ws://")) {
		log.Printf("[relay-node] WARNING: using non-TLS connection to %s — traffic is unencrypted", server)
	}

	if err := os.MkdirAll(dataPath, 0o755); err != nil {
		return fmt.Errorf("create data path: %w", err)
	}
	dataPath, err = canonicalizeNodeDataPath(dataPath)
	if err != nil {
		return err
	}

	fileops.AllowedRoots = []string{dataPath}
	log.Printf("[relay-node] file operations restricted to: %s", dataPath)

	// Fail closed before connecting: a node that cannot hand its drive
	// directories to the userns-remap base must never register and start
	// accepting launches, because every directory it created would be
	// unwritable by the agent containers that mount it.
	if err := configureDriveOwnership(driveOwnerFlag, dataPath); err != nil {
		return fmt.Errorf("drive ownership is not usable: %w", err)
	}
	if owner := fileops.ConfiguredDriveOwner(); owner != nil {
		log.Printf("[relay-node] drive directories under %v are owned by %s (docker userns-remap base)", fileops.DriveRoots(dataPath), owner)
	} else {
		log.Printf("[relay-node] drive ownership rewriting is disabled (--drive-owner %s); this node must not run dockerd with userns-remap", fileops.DriveOwnerNone)
	}

	fmt.Printf("Taurus Relay %s\n", health.Version)
	fmt.Printf("Starting node mode: %s (%s)\n", name, host)
	fmt.Printf("Connecting to %s...\n", server)

	tun := tunnel.NewNode(server, tunnel.NodeOptions{
		Name:          name,
		Host:          host,
		Token:         token,
		DataPath:      dataPath,
		MaxContainers: maxContainers,
		MaxSessions:   maxSessions,
	})

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("[relay-node] received %v, shutting down...", sig)
		tun.Stop()
	}()

	return tun.Run()
}

func validateNodePlatform(goos string) error {
	if goos == "windows" {
		return fmt.Errorf("taurus-relay node is not supported on Windows; Windows releases support connect mode only")
	}
	return nil
}

func canonicalizeNodeDataPath(dataPath string) (string, error) {
	absPath, err := filepath.Abs(dataPath)
	if err != nil {
		return "", fmt.Errorf("resolve data path %q: %w", dataPath, err)
	}
	resolvedPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return "", fmt.Errorf("canonicalize data path %q: %w", dataPath, err)
	}
	return resolvedPath, nil
}
