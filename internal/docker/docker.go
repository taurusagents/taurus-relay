package docker

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	StatusRunning  = "running"
	StatusPaused   = "paused"
	StatusStopped  = "stopped"
	StatusNotFound = "not_found"
)

type Client struct {
	DataPath string
	UseInit  bool
}

type ResourceLimits struct {
	CPUs      float64 `json:"cpus,omitempty"`
	MemoryMB  int     `json:"memory_mb,omitempty"`
	PidsLimit int     `json:"pids_limit,omitempty"`
}

type Mount struct {
	Host      string `json:"host"`
	Container string `json:"container"`
	Readonly  bool   `json:"readonly,omitempty"`
}

type EnsureOptions struct {
	ContainerID    string
	Image          string
	UserID         string
	AgentID        string
	RootAgentID    string
	ResourceLimits ResourceLimits
	Mounts         []Mount
}

func NewClient(dataPath string) *Client {
	return &Client{DataPath: dataPath, UseInit: true}
}

func (c *Client) docker(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return "", fmt.Errorf("docker %s: %s", strings.Join(args, " "), msg)
		}
		return "", fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (c *Client) ContainerStatus(containerID string) (string, error) {
	status, err := c.docker("inspect", "--format", "{{.State.Status}}", containerID)
	if err != nil {
		errText := strings.ToLower(err.Error())
		if strings.Contains(errText, "no such object") || strings.Contains(errText, "no such container") {
			return StatusNotFound, nil
		}
		return "", err
	}
	switch status {
	case "running":
		return StatusRunning, nil
	case "paused":
		return StatusPaused, nil
	default:
		return StatusStopped, nil
	}
}

func (c *Client) EnsureContainer(opts EnsureOptions) error {
	status, err := c.ContainerStatus(opts.ContainerID)
	if err != nil {
		return err
	}

	if status == StatusRunning {
		return nil
	}

	if status != StatusNotFound {
		if status == StatusPaused {
			_, err := c.docker("unpause", opts.ContainerID)
			return err
		}
		if _, err := c.docker("start", opts.ContainerID); err != nil {
			return err
		}
		return nil
	}

	if opts.Image == "" {
		return fmt.Errorf("image is required")
	}

	workspacePath := filepath.Join(c.DataPath, "taurus-drives", opts.UserID, opts.AgentID, "workspace")
	sharedPath := filepath.Join(c.DataPath, "taurus-drives", opts.UserID, opts.RootAgentID, "shared")
	if err := os.MkdirAll(workspacePath, 0o755); err != nil {
		return fmt.Errorf("create workspace path: %w", err)
	}
	if err := os.MkdirAll(sharedPath, 0o755); err != nil {
		return fmt.Errorf("create shared path: %w", err)
	}

	createArgs := []string{"create", "--name", opts.ContainerID, "-w", "/workspace"}
	if c.UseInit {
		createArgs = append(createArgs, "--init")
	}
	if opts.ResourceLimits.CPUs > 0 {
		createArgs = append(createArgs, "--cpus", strconv.FormatFloat(opts.ResourceLimits.CPUs, 'f', -1, 64))
	}
	if opts.ResourceLimits.MemoryMB > 0 {
		createArgs = append(createArgs, "--memory", fmt.Sprintf("%dm", opts.ResourceLimits.MemoryMB))
	}
	if opts.ResourceLimits.PidsLimit > 0 {
		createArgs = append(createArgs, "--pids-limit", strconv.Itoa(opts.ResourceLimits.PidsLimit))
	}

	createArgs = append(createArgs,
		"-v", fmt.Sprintf("%s:/workspace", workspacePath),
		"-v", fmt.Sprintf("%s:/shared", sharedPath),
	)

	for _, m := range opts.Mounts {
		spec := fmt.Sprintf("%s:%s", m.Host, m.Container)
		if m.Readonly {
			spec += ":ro"
		}
		createArgs = append(createArgs, "-v", spec)
	}

	createArgs = append(createArgs, opts.Image, "sleep", "infinity")
	if _, err := c.docker(createArgs...); err != nil {
		return err
	}
	if _, err := c.docker("start", opts.ContainerID); err != nil {
		return err
	}
	return nil
}

func (c *Client) Pause(containerID string) error {
	status, err := c.ContainerStatus(containerID)
	if err != nil {
		return err
	}
	if status == StatusRunning {
		_, err = c.docker("pause", containerID)
	}
	return err
}

func (c *Client) Unpause(containerID string) error {
	status, err := c.ContainerStatus(containerID)
	if err != nil {
		return err
	}
	if status == StatusPaused {
		_, err = c.docker("unpause", containerID)
	}
	return err
}

func (c *Client) Stop(containerID string) error {
	status, err := c.ContainerStatus(containerID)
	if err != nil {
		return err
	}
	if status == StatusRunning || status == StatusPaused {
		_, err = c.docker("stop", "-t", "5", containerID)
	}
	return err
}

func (c *Client) Destroy(containerID string) error {
	_, err := c.docker("rm", "-f", containerID)
	if err != nil && strings.Contains(err.Error(), "No such object") {
		return nil
	}
	return err
}

func (c *Client) RunningContainerCount() int {
	out, err := c.docker("ps", "-q")
	if err != nil || out == "" {
		return 0
	}
	return len(strings.Split(strings.TrimSpace(out), "\n"))
}
