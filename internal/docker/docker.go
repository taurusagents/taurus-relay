package docker

import (
	"fmt"
	"log"
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
		log.Printf("[relay-node/docker] container.ensure status lookup failed container=%s: %v", opts.ContainerID, err)
		return err
	}

	if status == StatusRunning {
		log.Printf("[relay-node/docker] container.ensure already running container=%s", opts.ContainerID)
		return nil
	}

	if status != StatusNotFound {
		if status == StatusPaused {
			log.Printf("[relay-node/docker] container.ensure unpausing existing container=%s", opts.ContainerID)
			_, err := c.docker("unpause", opts.ContainerID)
			if err != nil {
				log.Printf("[relay-node/docker] container.ensure unpause failed container=%s: %v", opts.ContainerID, err)
			}
			return err
		}
		log.Printf("[relay-node/docker] container.ensure starting existing container=%s status=%s", opts.ContainerID, status)
		if _, err := c.docker("start", opts.ContainerID); err != nil {
			log.Printf("[relay-node/docker] container.ensure start failed container=%s: %v", opts.ContainerID, err)
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
	log.Printf("[relay-node/docker] container.ensure creating container=%s image=%s user=%s agent=%s root=%s mounts=%d cpus=%.2f mem_mb=%d pids=%d",
		opts.ContainerID, opts.Image, opts.UserID, opts.AgentID, opts.RootAgentID, len(opts.Mounts),
		opts.ResourceLimits.CPUs, opts.ResourceLimits.MemoryMB, opts.ResourceLimits.PidsLimit,
	)
	if _, err := c.docker(createArgs...); err != nil {
		log.Printf("[relay-node/docker] container.ensure create failed container=%s: %v", opts.ContainerID, err)
		return err
	}
	if _, err := c.docker("start", opts.ContainerID); err != nil {
		log.Printf("[relay-node/docker] container.ensure start failed container=%s: %v", opts.ContainerID, err)
		return err
	}
	log.Printf("[relay-node/docker] container.ensure ready container=%s", opts.ContainerID)
	return nil
}

func (c *Client) Pause(containerID string) error {
	status, err := c.ContainerStatus(containerID)
	if err != nil {
		log.Printf("[relay-node/docker] container.pause status failed container=%s: %v", containerID, err)
		return err
	}
	if status == StatusRunning {
		log.Printf("[relay-node/docker] container.pause container=%s", containerID)
		_, err = c.docker("pause", containerID)
	}
	if err != nil {
		log.Printf("[relay-node/docker] container.pause failed container=%s: %v", containerID, err)
	}
	return err
}

func (c *Client) Unpause(containerID string) error {
	status, err := c.ContainerStatus(containerID)
	if err != nil {
		log.Printf("[relay-node/docker] container.unpause status failed container=%s: %v", containerID, err)
		return err
	}
	if status == StatusPaused {
		log.Printf("[relay-node/docker] container.unpause container=%s", containerID)
		_, err = c.docker("unpause", containerID)
	}
	if err != nil {
		log.Printf("[relay-node/docker] container.unpause failed container=%s: %v", containerID, err)
	}
	return err
}

func (c *Client) Stop(containerID string) error {
	status, err := c.ContainerStatus(containerID)
	if err != nil {
		log.Printf("[relay-node/docker] container.stop status failed container=%s: %v", containerID, err)
		return err
	}
	if status == StatusRunning || status == StatusPaused {
		log.Printf("[relay-node/docker] container.stop container=%s status=%s", containerID, status)
		_, err = c.docker("stop", "-t", "5", containerID)
	}
	if err != nil {
		log.Printf("[relay-node/docker] container.stop failed container=%s: %v", containerID, err)
	}
	return err
}

func (c *Client) Destroy(containerID string) error {
	log.Printf("[relay-node/docker] container.destroy container=%s", containerID)
	_, err := c.docker("rm", "-f", containerID)
	if err != nil {
		errText := strings.ToLower(err.Error())
		if strings.Contains(errText, "no such object") || strings.Contains(errText, "no such container") {
			return nil
		}
		log.Printf("[relay-node/docker] container.destroy failed container=%s: %v", containerID, err)
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
