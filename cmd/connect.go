// Package cmd implements CLI commands for taurus-relay.
package cmd

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/taurusagents/taurus-relay/internal/config"
	"github.com/taurusagents/taurus-relay/internal/fileops"
	"github.com/taurusagents/taurus-relay/internal/health"
	"github.com/taurusagents/taurus-relay/internal/tunnel"
)

// Connect handles the `taurus-relay connect` command.
func Connect(server, token string, insecure bool) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Server from flag overrides saved config
	if server != "" {
		cfg.Server = server
	}
	if cfg.Server == "" {
		return fmt.Errorf("--server is required (no saved server in config)")
	}

	// TLS enforcement: warn/error if using non-TLS
	if !insecure && (strings.HasPrefix(cfg.Server, "http://") || strings.HasPrefix(cfg.Server, "ws://")) {
		return fmt.Errorf("non-TLS server URL %q is not allowed without --insecure flag", cfg.Server)
	}
	if insecure && (strings.HasPrefix(cfg.Server, "http://") || strings.HasPrefix(cfg.Server, "ws://")) {
		log.Printf("[relay] WARNING: using non-TLS connection to %s — traffic is unencrypted", cfg.Server)
	}

	// If no token and no credentials, error
	if token == "" && !cfg.HasCredentials() {
		return fmt.Errorf("--token is required for first connection (no saved credentials)")
	}

	// Initialize file operation sandboxing to user's home directory
	homeDir, err := os.UserHomeDir()
	if err == nil {
		fileops.AllowedRoots = []string{homeDir}
		log.Printf("[relay] file operations restricted to: %s", homeDir)
	} else {
		return fmt.Errorf("could not determine home directory for file sandboxing: %v — refusing to start", err)
	}

	fmt.Printf("Taurus Relay %s\n", health.Version)
	fmt.Printf("Connecting to %s...\n", cfg.Server)

	tun := tunnel.New(cfg, token)

	// Handle graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("[relay] received %v, shutting down...", sig)
		tun.Stop()
	}()

	return tun.Run()
}

// Status shows relay connection status.
func Status() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if !cfg.HasCredentials() {
		fmt.Println("Not registered. Run 'taurus-relay connect --token <token> --server <url>' to register.")
		return nil
	}

	fmt.Printf("Server:    %s\n", cfg.Server)
	fmt.Printf("Target ID: %s\n", cfg.TargetID)
	fmt.Printf("Config:    %s\n", config.Path())
	fmt.Printf("Status:    Credentials saved (not checking live connection)\n")
	return nil
}

// Version prints version info.
func Version() {
	fmt.Printf("taurus-relay %s\n", health.Version)
}
