package cmd

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/taurusagents/taurus-relay/internal/fileops"
	"github.com/taurusagents/taurus-relay/internal/health"
	"github.com/taurusagents/taurus-relay/internal/tunnel"
)

func Node(server, name, host, token, dataPath string, maxContainers int, insecure bool) error {
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

	fileops.AllowedRoots = []string{dataPath}
	log.Printf("[relay-node] file operations restricted to: %s", dataPath)

	fmt.Printf("Taurus Relay %s\n", health.Version)
	fmt.Printf("Starting node mode: %s (%s)\n", name, host)
	fmt.Printf("Connecting to %s...\n", server)

	tun := tunnel.NewNode(server, tunnel.NodeOptions{
		Name:          name,
		Host:          host,
		Token:         token,
		DataPath:      dataPath,
		MaxContainers: maxContainers,
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
