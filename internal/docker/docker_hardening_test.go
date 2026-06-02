package docker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildTaurusManagedBridgeCreateArgs(t *testing.T) {
	got := buildTaurusManagedBridgeCreateArgs("taurus-node-bridge")
	want := []string{
		"network",
		"create",
		"--driver",
		"bridge",
		"--opt",
		"com.docker.network.bridge.enable_icc=false",
		"taurus-node-bridge",
	}
	if len(got) != len(want) {
		t.Fatalf("unexpected arg count: got %d want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg %d mismatch: got %q want %q (all args: %v)", i, got[i], want[i], got)
		}
	}
}

func TestBuildTaurusContainerHardeningArgs(t *testing.T) {
	got := buildTaurusContainerHardeningArgs("taurus-node-bridge")
	wantPrefix := []string{
		"--network", "taurus-node-bridge",
		"--security-opt", "no-new-privileges:true",
		"--cap-drop=ALL",
	}
	if len(got) != len(wantPrefix)+(len(taurusAgentCapabilityAllowlist)*2) {
		t.Fatalf("unexpected arg count: got %d args=%v", len(got), got)
	}
	for i := range wantPrefix {
		if got[i] != wantPrefix[i] {
			t.Fatalf("prefix arg %d mismatch: got %q want %q (all args: %v)", i, got[i], wantPrefix[i], got)
		}
	}
	for i, capability := range taurusAgentCapabilityAllowlist {
		base := len(wantPrefix) + (i * 2)
		if got[base] != "--cap-add" || got[base+1] != capability {
			t.Fatalf("capability pair %d mismatch: got %q %q want --cap-add %q (all args: %v)", i, got[base], got[base+1], capability, got)
		}
	}
}

func TestStrictTaurusContainerHardeningEnabled(t *testing.T) {
	t.Setenv("TAURUS_STRICT_CONTAINER_HARDENING", "")
	if strictTaurusContainerHardeningEnabled() {
		t.Fatalf("expected strict hardening enforcement to default off")
	}

	t.Setenv("TAURUS_STRICT_CONTAINER_HARDENING", "true")
	if !strictTaurusContainerHardeningEnabled() {
		t.Fatalf("expected strict hardening enforcement to turn on for true")
	}

	t.Setenv("TAURUS_STRICT_CONTAINER_HARDENING", "1")
	if !strictTaurusContainerHardeningEnabled() {
		t.Fatalf("expected strict hardening enforcement to turn on for 1")
	}

	t.Setenv("TAURUS_STRICT_CONTAINER_HARDENING", "false")
	if strictTaurusContainerHardeningEnabled() {
		t.Fatalf("expected strict hardening enforcement to stay off for false")
	}
}

func TestDescribeTaurusManagedBridgeNetworkMismatch(t *testing.T) {
	if mismatch := describeTaurusManagedBridgeNetworkMismatch(
		taurusManagedBridgeNetworkInspect{
			Name:   "taurus-node-bridge",
			Driver: "bridge",
			Options: map[string]string{
				"com.docker.network.bridge.enable_icc": "false",
			},
		},
		"taurus-node-bridge",
	); mismatch != "" {
		t.Fatalf("expected no mismatch, got %q", mismatch)
	}

	mismatch := describeTaurusManagedBridgeNetworkMismatch(
		taurusManagedBridgeNetworkInspect{
			Name:   "taurus-node-bridge",
			Driver: "overlay",
			Options: map[string]string{
				"com.docker.network.bridge.enable_icc": "true",
			},
		},
		"taurus-node-bridge",
	)
	if mismatch == "" || mismatch == "taurus-node-bridge" {
		t.Fatalf("expected descriptive mismatch, got %q", mismatch)
	}
}

func TestDescribeTaurusContainerHardeningDrift(t *testing.T) {
	if drift := describeTaurusContainerHardeningDrift(
		taurusContainerHardeningInspect{
			Name: "/taurus-agent-demo",
			HostConfig: struct {
				NetworkMode string   `json:"NetworkMode"`
				Privileged  bool     `json:"Privileged"`
				SecurityOpt []string `json:"SecurityOpt"`
				CapDrop     []string `json:"CapDrop"`
				CapAdd      []string `json:"CapAdd"`
			}{
				NetworkMode: "taurus-node-bridge",
				Privileged:  false,
				SecurityOpt: []string{"no-new-privileges:true"},
				CapDrop:     []string{"ALL"},
				CapAdd:      append([]string(nil), taurusAgentCapabilityAllowlist...),
			},
			NetworkSettings: struct {
				Networks map[string]json.RawMessage `json:"Networks"`
			}{
				Networks: map[string]json.RawMessage{
					"taurus-node-bridge": json.RawMessage(`{}`),
				},
			},
		},
		"taurus-node-bridge",
	); len(drift) != 0 {
		t.Fatalf("expected no drift, got %v", drift)
	}

	drift := describeTaurusContainerHardeningDrift(
		taurusContainerHardeningInspect{
			Name: "/taurus-agent-demo",
			HostConfig: struct {
				NetworkMode string   `json:"NetworkMode"`
				Privileged  bool     `json:"Privileged"`
				SecurityOpt []string `json:"SecurityOpt"`
				CapDrop     []string `json:"CapDrop"`
				CapAdd      []string `json:"CapAdd"`
			}{
				NetworkMode: "bridge",
				Privileged:  true,
				CapAdd:      []string{"CHOWN", "SYS_ADMIN"},
			},
			NetworkSettings: struct {
				Networks map[string]json.RawMessage `json:"Networks"`
			}{
				Networks: map[string]json.RawMessage{
					"bridge":             json.RawMessage(`{}`),
					"taurus-node-bridge": json.RawMessage(`{}`),
					"sideways":           json.RawMessage(`{}`),
				},
			},
		},
		"taurus-node-bridge",
	)
	wantSubstrings := []string{
		`uses network mode "bridge" instead of "taurus-node-bridge"`,
		"attached to unexpected Docker networks: bridge, sideways",
		"is privileged",
		"missing security-opt no-new-privileges:true",
		"missing cap-drop ALL",
		"missing allowed capabilities",
		"grants unexpected capabilities: SYS_ADMIN",
	}
	for _, want := range wantSubstrings {
		found := false
		for _, reason := range drift {
			if strings.Contains(reason, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected drift to contain %q, got %v", want, drift)
		}
	}
}

func TestValidateBindMounts(t *testing.T) {
	valid := []Mount{{Host: "/host/path", Container: "/container/path", Readonly: true}}
	if err := validateBindMounts(valid); err != nil {
		t.Fatalf("expected valid mounts, got %v", err)
	}

	tests := []struct {
		name   string
		mounts []Mount
		want   string
	}{
		{
			name:   "rejects host colon",
			mounts: []Mount{{Host: "/tmp:bad", Container: "/dst"}},
			want:   "Invalid characters in bind mount host path: /tmp:bad",
		},
		{
			name:   "rejects container null byte",
			mounts: []Mount{{Host: "/tmp", Container: "/dst\x00bad"}},
			want:   "Invalid characters in bind mount container path: /dst\x00bad",
		},
		{
			name:   "rejects relative host path",
			mounts: []Mount{{Host: "relative", Container: "/dst"}},
			want:   "Bind mount host path must be absolute: relative",
		},
		{
			name:   "rejects relative container path",
			mounts: []Mount{{Host: "/tmp", Container: "relative"}},
			want:   "Bind mount container path must be absolute: relative",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBindMounts(tc.mounts)
			if err == nil {
				t.Fatalf("expected error %q, got nil", tc.want)
			}
			if err.Error() != tc.want {
				t.Fatalf("unexpected error: got %q want %q", err.Error(), tc.want)
			}
		})
	}
}

func TestEnsureContainerRolloutSafeReusesExistingDriftedContainerWithoutNetworkValidation(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "docker.log")
	dockerPath := filepath.Join(tmpDir, "docker")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_DOCKER_LOG"
if [ "$1" = "inspect" ] && [ "$2" = "--format" ] && [ "$3" = "{{json .}}" ] && [ "$4" = "taurus-agent-demo" ]; then
  cat <<'JSON'
{"Name":"/taurus-agent-demo","State":{"Status":"running"},"HostConfig":{"NetworkMode":"bridge","Privileged":true,"SecurityOpt":[],"CapDrop":[],"CapAdd":["CHOWN"]},"NetworkSettings":{"Networks":{"bridge":{}}}}
JSON
  exit 0
fi
if [ "$1" = "network" ] && [ "$2" = "inspect" ]; then
  echo "network inspect should not be called" >&2
  exit 1
fi
echo "unexpected docker args: $*" >&2
exit 1
`
	if err := os.WriteFile(dockerPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker shim: %v", err)
	}

	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DOCKER_LOG", logPath)
	t.Setenv("TAURUS_STRICT_CONTAINER_HARDENING", "0")

	client := NewClient(tmpDir)
	if err := client.EnsureContainer(EnsureOptions{ContainerID: "taurus-agent-demo"}); err != nil {
		t.Fatalf("expected rollout-safe ensure to reuse existing container, got %v", err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake docker log: %v", err)
	}
	if strings.Contains(string(logBytes), "network inspect") {
		t.Fatalf("expected rollout-safe reuse to skip network validation, got log %q", string(logBytes))
	}
}

func TestEnsureContainerStrictModeStillValidatesExistingNetwork(t *testing.T) {
	tmpDir := t.TempDir()
	dockerPath := filepath.Join(tmpDir, "docker")
	script := `#!/bin/sh
if [ "$1" = "network" ] && [ "$2" = "inspect" ] && [ "$5" = "taurus-node-bridge" ]; then
  cat <<'JSON'
{"Name":"taurus-node-bridge","Driver":"bridge","Options":{"com.docker.network.bridge.enable_icc":"true"}}
JSON
  exit 0
fi
if [ "$1" = "inspect" ]; then
  echo "container inspect should not be reached before strict network validation" >&2
  exit 1
fi
echo "unexpected docker args: $*" >&2
exit 1
`
	if err := os.WriteFile(dockerPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker shim: %v", err)
	}

	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TAURUS_STRICT_CONTAINER_HARDENING", "1")

	client := NewClient(tmpDir)
	err := client.EnsureContainer(EnsureOptions{ContainerID: "taurus-agent-demo"})
	if err == nil {
		t.Fatalf("expected strict ensure to reject mismatched existing network")
	}
	if !strings.Contains(err.Error(), "refusing to use pre-existing Docker network") {
		t.Fatalf("unexpected strict ensure error: %v", err)
	}
}
