package docker

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	StatusRunning                     = "running"
	StatusPaused                      = "paused"
	StatusStopped                     = "stopped"
	StatusNotFound                    = "not_found"
	defaultTaurusContainerNetworkName = "taurus-node-bridge"
)

var taurusAgentCapabilityAllowlist = []string{
	"CHOWN",
	"DAC_OVERRIDE",
	"FOWNER",
	"FSETID",
	"KILL",
	"SETGID",
	"SETUID",
	"SETPCAP",
	"NET_BIND_SERVICE",
	"SYS_CHROOT",
	"AUDIT_WRITE",
	"SETFCAP",
}

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

type taurusManagedBridgeNetworkInspect struct {
	Name    string            `json:"Name"`
	Driver  string            `json:"Driver"`
	Options map[string]string `json:"Options"`
}

type taurusContainerHardeningInspect struct {
	Name  string `json:"Name"`
	State struct {
		Status string `json:"Status"`
	} `json:"State"`
	HostConfig struct {
		NetworkMode string   `json:"NetworkMode"`
		Privileged  bool     `json:"Privileged"`
		SecurityOpt []string `json:"SecurityOpt"`
		CapDrop     []string `json:"CapDrop"`
		CapAdd      []string `json:"CapAdd"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]json.RawMessage `json:"Networks"`
	} `json:"NetworkSettings"`
}

func NewClient(dataPath string) *Client {
	return &Client{DataPath: dataPath, UseInit: true}
}

func taurusContainerNetworkName() string {
	configuredName := strings.TrimSpace(os.Getenv("TAURUS_DOCKER_NETWORK_NAME"))
	if configuredName != "" {
		return configuredName
	}
	return defaultTaurusContainerNetworkName
}

func buildTaurusManagedBridgeCreateArgs(networkName string) []string {
	return []string{
		"network",
		"create",
		"--driver",
		"bridge",
		"--opt",
		"com.docker.network.bridge.enable_icc=false",
		networkName,
	}
}

func buildTaurusContainerHardeningArgs(networkName string) []string {
	args := []string{
		"--network", networkName,
		"--security-opt", "no-new-privileges:true",
		"--cap-drop=ALL",
	}
	for _, capability := range taurusAgentCapabilityAllowlist {
		args = append(args, "--cap-add", capability)
	}
	return args
}

func dockerErrorIndicatesMissingObject(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no such container") ||
		strings.Contains(message, "no such object") ||
		strings.Contains(message, "no such network")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func formatInspectTarget(kind string, rawName string) string {
	name := strings.TrimPrefix(strings.TrimSpace(rawName), "/")
	if name == "" {
		return fmt.Sprintf("Docker %s", kind)
	}
	return fmt.Sprintf("%s %s", kind, name)
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func sortedNetworkNames(networks map[string]json.RawMessage) []string {
	if len(networks) == 0 {
		return nil
	}
	names := make([]string, 0, len(networks))
	for networkName := range networks {
		names = append(names, networkName)
	}
	sort.Strings(names)
	return names
}

func describeTaurusManagedBridgeNetworkMismatch(inspect taurusManagedBridgeNetworkInspect, expectedNetworkName string) string {
	problems := make([]string, 0, 2)
	actualName := formatInspectTarget("network", firstNonEmpty(inspect.Name, expectedNetworkName))
	if inspect.Driver != "bridge" {
		problems = append(problems, fmt.Sprintf("uses driver %q instead of %q", inspect.Driver, "bridge"))
	}
	if inspect.Options["com.docker.network.bridge.enable_icc"] != "false" {
		problems = append(
			problems,
			fmt.Sprintf(
				"has com.docker.network.bridge.enable_icc=%q instead of %q",
				inspect.Options["com.docker.network.bridge.enable_icc"],
				"false",
			),
		)
	}
	if len(problems) == 0 {
		return ""
	}
	return fmt.Sprintf("%s %s", actualName, strings.Join(problems, " and "))
}

func describeTaurusContainerHardeningDrift(inspect taurusContainerHardeningInspect, expectedNetworkName string) []string {
	drift := make([]string, 0)
	containerName := formatInspectTarget("container", inspect.Name)
	if inspect.HostConfig.NetworkMode != expectedNetworkName {
		drift = append(
			drift,
			fmt.Sprintf("%s uses network mode %q instead of %q", containerName, inspect.HostConfig.NetworkMode, expectedNetworkName),
		)
	}
	// NetworkMode only captures the container's primary network. Docker can attach
	// additional networks after creation, so inspect the live attachment set before
	// trusting a pre-existing container as Taurus-hardened.
	attachedNetworks := sortedNetworkNames(inspect.NetworkSettings.Networks)
	unexpectedNetworks := make([]string, 0)
	attachedToExpectedNetwork := false
	for _, networkName := range attachedNetworks {
		if networkName == expectedNetworkName {
			attachedToExpectedNetwork = true
			continue
		}
		unexpectedNetworks = append(unexpectedNetworks, networkName)
	}
	if len(unexpectedNetworks) > 0 {
		drift = append(drift, fmt.Sprintf("%s is attached to unexpected Docker networks: %s", containerName, strings.Join(unexpectedNetworks, ", ")))
	}
	if len(attachedNetworks) > 0 && !attachedToExpectedNetwork {
		drift = append(drift, fmt.Sprintf("%s is not attached to the Taurus-managed Docker network %q", containerName, expectedNetworkName))
	}
	if inspect.HostConfig.Privileged {
		drift = append(drift, fmt.Sprintf("%s is privileged", containerName))
	}
	if !containsString(inspect.HostConfig.SecurityOpt, "no-new-privileges:true") {
		drift = append(drift, fmt.Sprintf("%s is missing security-opt no-new-privileges:true", containerName))
	}
	if !containsString(inspect.HostConfig.CapDrop, "ALL") {
		drift = append(drift, fmt.Sprintf("%s is missing cap-drop ALL", containerName))
	}

	capAdd := make(map[string]struct{}, len(inspect.HostConfig.CapAdd))
	for _, capability := range inspect.HostConfig.CapAdd {
		capAdd[capability] = struct{}{}
	}
	missingCaps := make([]string, 0)
	for _, capability := range taurusAgentCapabilityAllowlist {
		if _, ok := capAdd[capability]; !ok {
			missingCaps = append(missingCaps, capability)
		}
	}
	if len(missingCaps) > 0 {
		drift = append(drift, fmt.Sprintf("%s is missing allowed capabilities: %s", containerName, strings.Join(missingCaps, ", ")))
	}

	allowlist := make(map[string]struct{}, len(taurusAgentCapabilityAllowlist))
	for _, capability := range taurusAgentCapabilityAllowlist {
		allowlist[capability] = struct{}{}
	}
	unexpectedCaps := make([]string, 0)
	for capability := range capAdd {
		if _, ok := allowlist[capability]; !ok {
			unexpectedCaps = append(unexpectedCaps, capability)
		}
	}
	sort.Strings(unexpectedCaps)
	if len(unexpectedCaps) > 0 {
		drift = append(drift, fmt.Sprintf("%s grants unexpected capabilities: %s", containerName, strings.Join(unexpectedCaps, ", ")))
	}

	return drift
}

func validateBindMounts(mounts []Mount) error {
	for _, mount := range mounts {
		// The control plane already type-checks mount objects before sending them to
		// Relay, but Relay still needs to reject dangerous path contents before
		// interpolating them into Docker's `-v source:dest[:opts]` syntax.
		if strings.Contains(mount.Host, ":") || strings.ContainsRune(mount.Host, '\x00') {
			return fmt.Errorf("Invalid characters in bind mount host path: %s", mount.Host)
		}
		if strings.Contains(mount.Container, ":") || strings.ContainsRune(mount.Container, '\x00') {
			return fmt.Errorf("Invalid characters in bind mount container path: %s", mount.Container)
		}
		if !filepath.IsAbs(mount.Host) {
			return fmt.Errorf("Bind mount host path must be absolute: %s", mount.Host)
		}
		if !filepath.IsAbs(mount.Container) {
			return fmt.Errorf("Bind mount container path must be absolute: %s", mount.Container)
		}
	}
	return nil
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

func (c *Client) inspectAgentNetwork(networkName string) (*taurusManagedBridgeNetworkInspect, error) {
	raw, err := c.docker("network", "inspect", "--format", "{{json .}}", networkName)
	if err != nil {
		if dockerErrorIndicatesMissingObject(err) {
			return nil, nil
		}
		return nil, err
	}
	inspect := &taurusManagedBridgeNetworkInspect{}
	if err := json.Unmarshal([]byte(raw), inspect); err != nil {
		return nil, fmt.Errorf("docker network inspect %s returned invalid JSON: %w", networkName, err)
	}
	return inspect, nil
}

func (c *Client) inspectContainerHardening(containerID string) (*taurusContainerHardeningInspect, error) {
	raw, err := c.docker("inspect", "--format", "{{json .}}", containerID)
	if err != nil {
		if dockerErrorIndicatesMissingObject(err) {
			return nil, nil
		}
		return nil, err
	}
	inspect := &taurusContainerHardeningInspect{}
	if err := json.Unmarshal([]byte(raw), inspect); err != nil {
		return nil, fmt.Errorf("docker inspect %s returned invalid JSON: %w", containerID, err)
	}
	return inspect, nil
}

func (c *Client) ensureAgentNetwork() (string, error) {
	networkName := taurusContainerNetworkName()
	existingNetwork, err := c.inspectAgentNetwork(networkName)
	if err != nil {
		return "", err
	}
	if existingNetwork != nil {
		if mismatch := describeTaurusManagedBridgeNetworkMismatch(*existingNetwork, networkName); mismatch != "" {
			return "", fmt.Errorf(
				"refusing to use pre-existing Docker network %q because %s; recreate that network with Taurus-managed settings or choose a different TAURUS_DOCKER_NETWORK_NAME",
				networkName,
				mismatch,
			)
		}
		return networkName, nil
	}

	if _, err := c.docker(buildTaurusManagedBridgeCreateArgs(networkName)...); err != nil {
		// Another launcher may have created the network after our initial inspect.
		existingNetwork, inspectErr := c.inspectAgentNetwork(networkName)
		if inspectErr != nil {
			return "", inspectErr
		}
		if existingNetwork == nil {
			return "", err
		}
	}

	finalNetwork, err := c.inspectAgentNetwork(networkName)
	if err != nil {
		return "", err
	}
	if finalNetwork == nil {
		return "", fmt.Errorf("Docker reported Taurus-managed network %q exists, but a follow-up inspect could not read it", networkName)
	}
	if mismatch := describeTaurusManagedBridgeNetworkMismatch(*finalNetwork, networkName); mismatch != "" {
		return "", fmt.Errorf(
			"refusing to use pre-existing Docker network %q because %s; recreate that network with Taurus-managed settings or choose a different TAURUS_DOCKER_NETWORK_NAME",
			networkName,
			mismatch,
		)
	}

	log.Printf("[relay-node/docker] created Taurus-managed Docker bridge network=%s", networkName)
	return networkName, nil
}

func (c *Client) ContainerStatus(containerID string) (string, error) {
	status, err := c.docker("inspect", "--format", "{{.State.Status}}", containerID)
	if err != nil {
		if dockerErrorIndicatesMissingObject(err) {
			return StatusNotFound, nil
		}
		return "", err
	}
	return normalizeContainerStatus(status), nil
}

func normalizeContainerStatus(raw string) string {
	switch raw {
	case "running":
		return StatusRunning
	case "paused":
		return StatusPaused
	case "":
		return StatusNotFound
	default:
		return StatusStopped
	}
}

func (c *Client) EnsureContainer(opts EnsureOptions) error {
	networkName, err := c.ensureAgentNetwork()
	if err != nil {
		return fmt.Errorf("ensure Taurus agent network: %w", err)
	}

	inspect, err := c.inspectContainerHardening(opts.ContainerID)
	if err != nil {
		log.Printf("[relay-node/docker] container.ensure inspect failed container=%s: %v", opts.ContainerID, err)
		return err
	}
	if inspect != nil {
		status := normalizeContainerStatus(inspect.State.Status)
		drift := describeTaurusContainerHardeningDrift(*inspect, networkName)
		if len(drift) == 0 {
			if status == StatusRunning {
				log.Printf("[relay-node/docker] container.ensure already running container=%s", opts.ContainerID)
				return nil
			}
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

		if status == StatusRunning || status == StatusPaused {
			driftErr := fmt.Errorf(
				"existing container %s does not match current Taurus hardening requirements (%s); refusing to reuse status=%s. Stop/destroy the container so Taurus can recreate it with the current security settings",
				opts.ContainerID,
				strings.Join(drift, "; "),
				status,
			)
			log.Printf("[relay-node/docker] container.ensure hardening drift container=%s: %v", opts.ContainerID, driftErr)
			return driftErr
		}

		log.Printf(
			"[relay-node/docker] container.ensure removing stopped container=%s so Taurus can recreate it with current hardening: %s",
			opts.ContainerID,
			strings.Join(drift, "; "),
		)
		if _, err := c.docker("rm", "-f", opts.ContainerID); err != nil {
			log.Printf("[relay-node/docker] container.ensure remove failed container=%s: %v", opts.ContainerID, err)
			return err
		}
	}

	if opts.Image == "" {
		return fmt.Errorf("image is required")
	}
	if err := validateBindMounts(opts.Mounts); err != nil {
		return err
	}

	workspacePath := filepath.Join(c.DataPath, "taurus-drives", opts.UserID, opts.AgentID, "workspace")
	sharedPath := filepath.Join(c.DataPath, "taurus-drives", opts.UserID, opts.RootAgentID, "shared")
	if err := os.MkdirAll(workspacePath, 0o755); err != nil {
		return fmt.Errorf("create workspace path: %w", err)
	}
	if err := os.MkdirAll(sharedPath, 0o755); err != nil {
		return fmt.Errorf("create shared path: %w", err)
	}

	createArgs := []string{"create", "--name", opts.ContainerID}
	createArgs = append(createArgs, buildTaurusContainerHardeningArgs(networkName)...)
	createArgs = append(createArgs, "-w", "/workspace")
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

func (c *Client) ExecCommand(containerID string, command []string) (string, error) {
	args := append([]string{"exec", containerID}, command...)
	log.Printf("[relay-node/docker] exec_command container=%s command=%v", containerID, command)
	out, err := c.docker(args...)
	if err != nil {
		log.Printf("[relay-node/docker] exec_command failed container=%s: %v", containerID, err)
		return "", err
	}
	return out, nil
}

func (c *Client) ExecWithStdin(containerID string, command []string, stdin string) (string, error) {
	args := append([]string{"exec", "-i", containerID}, command...)
	log.Printf("[relay-node/docker] exec_with_stdin container=%s command=%v stdin_len=%d", containerID, command, len(stdin))
	cmd := exec.Command("docker", args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		log.Printf("[relay-node/docker] exec_with_stdin failed container=%s: %s", containerID, msg)
		return "", fmt.Errorf("docker exec: %s", msg)
	}
	return strings.TrimSpace(string(out)), nil
}

func (c *Client) RunningContainerCount() int {
	out, err := c.docker("ps", "-q")
	if err != nil || out == "" {
		return 0
	}
	return len(strings.Split(strings.TrimSpace(out), "\n"))
}
