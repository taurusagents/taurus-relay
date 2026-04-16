package tunnel

import (
	"strings"
	"testing"

	dockermux "github.com/taurusagents/taurus-relay/internal/docker"
)

func TestWithContainerMutationBlocksExecDuringAction(t *testing.T) {
	tun := &Tunnel{execs: dockermux.NewExecMultiplexer(nil, nil)}
	containerID := "container-1"
	actionRan := false

	err := tun.withContainerMutation(containerID, "container.stop", func() error {
		actionRan = true
		err := tun.execs.Create(containerID, "session-1", "bash", nil, "", nil, false, 0, 0)
		if err == nil {
			t.Fatalf("expected exec create to fail while mutation is active")
		}
		if !strings.Contains(err.Error(), "lifecycle transition") {
			t.Fatalf("expected lifecycle transition error, got: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withContainerMutation returned error: %v", err)
	}
	if !actionRan {
		t.Fatalf("expected lifecycle action to run")
	}
}

func TestWithContainerMutationRunsActionWithoutExecMux(t *testing.T) {
	tun := &Tunnel{}
	called := false

	err := tun.withContainerMutation("container-1", "container.stop", func() error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("withContainerMutation returned error: %v", err)
	}
	if !called {
		t.Fatalf("expected action to run")
	}
}
