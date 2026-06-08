package tunnel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/taurusagents/taurus-relay/internal/config"
	"github.com/taurusagents/taurus-relay/internal/protocol"
)

func waitForCondition(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if fn() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %v", timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	var pid int
	waitForCondition(t, time.Second, func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		parsed, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			t.Fatalf("parse pid %q: %v", string(data), err)
		}
		pid = parsed
		return pid > 0
	})
	return pid
}

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func beginBlockedRuntimeReset(t *testing.T, tun *Tunnel) chan struct{} {
	t.Helper()
	startGeneration := tun.currentRuntimeGeneration()
	tun.outputMu.Lock()
	done := make(chan struct{})
	go func() {
		tun.resetRuntimeState()
		close(done)
	}()
	waitForCondition(t, time.Second, func() bool {
		return tun.currentRuntimeGeneration() == startGeneration+1
	})
	select {
	case <-done:
		t.Fatal("reset completed before output teardown barrier was released")
	default:
	}
	return done
}

func finishBlockedRuntimeReset(t *testing.T, tun *Tunnel, done chan struct{}) {
	t.Helper()
	tun.outputMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("resetRuntimeState did not complete after releasing output teardown barrier")
	}
}

func installBlockingRipgrep(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	pidPath := filepath.Join(t.TempDir(), "rg.pid")
	scriptPath := filepath.Join(binDir, "rg")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$$\" > \"$RG_PID\"\n" +
		"while :; do sleep 1; done\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write blocking rg: %v", err)
	}
	t.Setenv("RG_PID", pidPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return pidPath
}

func TestEnqueueControlRoutesPriorityLane(t *testing.T) {
	tun := &Tunnel{
		ctx:              context.Background(),
		priorityControlQ: make(chan *protocol.Message, 1),
		normalControlQ:   make(chan *protocol.Message, 1),
	}

	tun.enqueueControl(&protocol.Message{Type: "priority", Priority: protocol.PriorityPriority})
	tun.enqueueControl(&protocol.Message{Type: "normal"})

	select {
	case msg := <-tun.priorityControlQ:
		if msg.Type != "priority" {
			t.Fatalf("expected priority message, got %s", msg.Type)
		}
	default:
		t.Fatalf("expected priority control message")
	}

	select {
	case msg := <-tun.normalControlQ:
		if msg.Type != "normal" {
			t.Fatalf("expected normal message, got %s", msg.Type)
		}
	default:
		t.Fatalf("expected normal control message")
	}
}

func TestDequeueOutputForPriorityPrefersPrioritySessions(t *testing.T) {
	tun := &Tunnel{
		ctx:          context.Background(),
		outputQueues: make(map[string]*sessionOutputQueue),
		outputReady:  make(chan struct{}, 1),
	}
	tun.runtimeGeneration.Store(1)

	tun.enqueueOutput("normal-session", protocol.PriorityNormal, &protocol.Message{Type: "normal-output"}, 1)
	tun.enqueueOutput("priority-session", protocol.PriorityPriority, &protocol.Message{Type: "priority-output"}, 1)

	msg, ok := tun.dequeueOutputForPriority(protocol.PriorityPriority)
	if !ok {
		t.Fatalf("expected priority output to be available")
	}
	if msg.Type != "priority-output" {
		t.Fatalf("expected priority output first, got %s", msg.Type)
	}

	msg, ok = tun.dequeueOutputForPriority(protocol.PriorityNormal)
	if !ok {
		t.Fatalf("expected normal output to remain available")
	}
	if msg.Type != "normal-output" {
		t.Fatalf("expected normal output second, got %s", msg.Type)
	}
}

func TestTryDequeueNextMessagePreventsNormalStarvation(t *testing.T) {
	tun := &Tunnel{
		ctx:              context.Background(),
		priorityControlQ: make(chan *protocol.Message, priorityBurstLimit+1),
		normalControlQ:   make(chan *protocol.Message, 1),
		outputQueues:     make(map[string]*sessionOutputQueue),
		outputReady:      make(chan struct{}, 1),
	}
	tun.runtimeGeneration.Store(1)

	for i := 0; i < priorityBurstLimit+1; i++ {
		tun.enqueueControl(&protocol.Message{Type: "priority", Priority: protocol.PriorityPriority})
	}
	tun.enqueueControl(&protocol.Message{Type: "normal", Priority: protocol.PriorityNormal})

	consecutivePriority := 0
	for i := 0; i < priorityBurstLimit; i++ {
		msg, ok := tun.tryDequeueNextMessage(consecutivePriority)
		if !ok {
			t.Fatalf("expected queued message at iteration %d", i)
		}
		if msg.Type != "priority" {
			t.Fatalf("expected priority message before fairness kick-in, got %s", msg.Type)
		}
		consecutivePriority++
	}

	msg, ok := tun.tryDequeueNextMessage(consecutivePriority)
	if !ok {
		t.Fatalf("expected fairness dequeue after priority burst")
	}
	if msg.Type != "normal" {
		t.Fatalf("expected normal message after %d priority sends, got %s", priorityBurstLimit, msg.Type)
	}
}

func TestCompletedOutputQueueIsDeletedAfterDrain(t *testing.T) {
	tun := &Tunnel{
		ctx:                     context.Background(),
		outputQueues:            make(map[string]*sessionOutputQueue),
		completedOutputSessions: make(map[string]struct{}),
		outputReady:             make(chan struct{}, 1),
	}
	tun.runtimeGeneration.Store(1)

	tun.enqueueOutput("completed-session", protocol.PriorityNormal, &protocol.Message{Type: "chunk"}, 1)
	tun.markOutputSessionComplete("completed-session")

	msg, ok := tun.dequeueOutputForPriority(protocol.PriorityNormal)
	if !ok || msg.Type != "chunk" {
		t.Fatalf("expected queued output before cleanup, got msg=%v ok=%t", msg, ok)
	}
	if _, ok := tun.outputQueues["completed-session"]; ok {
		t.Fatalf("expected drained output queue to be deleted")
	}
	if _, ok := tun.completedOutputSessions["completed-session"]; ok {
		t.Fatalf("expected completed output session marker to be deleted")
	}
}

func TestCompletedOutputQueueCreatedAfterExitStillDeletesAfterDrain(t *testing.T) {
	tun := &Tunnel{
		ctx:                     context.Background(),
		outputQueues:            make(map[string]*sessionOutputQueue),
		completedOutputSessions: make(map[string]struct{}),
		outputReady:             make(chan struct{}, 1),
	}
	tun.runtimeGeneration.Store(1)

	tun.markOutputSessionComplete("late-output-session")
	tun.enqueueOutput("late-output-session", protocol.PriorityNormal, &protocol.Message{Type: "chunk"}, 1)

	msg, ok := tun.dequeueOutputForPriority(protocol.PriorityNormal)
	if !ok || msg.Type != "chunk" {
		t.Fatalf("expected queued late output before cleanup, got msg=%v ok=%t", msg, ok)
	}
	if _, ok := tun.outputQueues["late-output-session"]; ok {
		t.Fatalf("expected late output queue to be deleted after drain")
	}
	if _, ok := tun.completedOutputSessions["late-output-session"]; ok {
		t.Fatalf("expected completed marker for late output queue to be deleted")
	}
}

func TestOutputQueuesAreScopedBySessionNamespace(t *testing.T) {
	tun := &Tunnel{
		ctx:                     context.Background(),
		outputQueues:            make(map[string]*sessionOutputQueue),
		completedOutputSessions: make(map[string]struct{}),
		outputReady:             make(chan struct{}, 1),
	}
	tun.runtimeGeneration.Store(1)

	tun.enqueueOutputForSession(outputSessionNamespaceProc, "shared-session", protocol.PriorityNormal, &protocol.Message{Type: "proc-output"}, 1)
	tun.enqueueOutputForSession(outputSessionNamespaceShell, "shared-session", protocol.PriorityNormal, &protocol.Message{Type: "shell-output"}, 1)
	tun.markOutputSessionCompleteFor(outputSessionNamespaceProc, "shared-session")
	tun.markOutputSessionCompleteFor(outputSessionNamespaceShell, "shared-session")

	if len(tun.outputQueues) != 2 {
		t.Fatalf("expected separate queues per namespace, got %d", len(tun.outputQueues))
	}
	if !tun.outputSessionStateExists(outputSessionNamespaceProc, "shared-session") {
		t.Fatalf("expected proc namespace state to exist")
	}
	if !tun.outputSessionStateExists(outputSessionNamespaceShell, "shared-session") {
		t.Fatalf("expected shell namespace state to exist")
	}

	seen := map[string]bool{}
	for len(seen) < 2 {
		msg, ok := tun.dequeueOutputForPriority(protocol.PriorityNormal)
		if !ok {
			t.Fatalf("expected two queued namespace-scoped messages, saw %v", seen)
		}
		seen[msg.Type] = true
	}
	if !seen["proc-output"] || !seen["shell-output"] {
		t.Fatalf("expected both proc and shell output messages, saw %v", seen)
	}
	if tun.outputSessionStateExists(outputSessionNamespaceProc, "shared-session") {
		t.Fatalf("expected proc namespace state to clear after drain")
	}
	if tun.outputSessionStateExists(outputSessionNamespaceShell, "shared-session") {
		t.Fatalf("expected shell namespace state to clear after drain")
	}
}

func TestResetRuntimeStateReleasesBlockedOutputProducer(t *testing.T) {
	tun := &Tunnel{
		ctx:              context.Background(),
		priorityControlQ: make(chan *protocol.Message, 1),
		normalControlQ:   make(chan *protocol.Message, 1),
		outputQueues:     make(map[string]*sessionOutputQueue),
		outputReady:      make(chan struct{}, 1),
	}
	tun.runtimeGeneration.Store(1)

	for i := 0; i < sessionQueueMaxMessages; i++ {
		tun.enqueueOutput("blocked-session", protocol.PriorityNormal, &protocol.Message{Type: "fill"}, 1)
	}

	done := make(chan struct{})
	go func() {
		tun.enqueueOutputForGeneration(1, "blocked-session", protocol.PriorityNormal, &protocol.Message{Type: "blocked"}, 1)
		close(done)
	}()

	select {
	case <-done:
		t.Fatalf("expected producer to block before reset")
	case <-time.After(50 * time.Millisecond):
	}

	tun.resetRuntimeState()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("blocked producer did not exit after reset")
	}
}

func TestEnqueueControlForGenerationDropsBlockedStaleSendAfterReset(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtimeCtx, runtimeCancel := context.WithCancel(ctx)
	defer runtimeCancel()

	tun := &Tunnel{
		ctx:              ctx,
		priorityControlQ: make(chan *protocol.Message, 1),
		normalControlQ:   make(chan *protocol.Message, 1),
		outputQueues:     make(map[string]*sessionOutputQueue),
		outputReady:      make(chan struct{}, 1),
		runtimeCtx:       runtimeCtx,
		runtimeCancel:    runtimeCancel,
	}
	tun.runtimeGeneration.Store(1)
	tun.priorityControlQ <- &protocol.Message{Type: "fill", Priority: protocol.PriorityPriority}

	done := make(chan struct{})
	go func() {
		tun.enqueueControlForGeneration(1, &protocol.Message{Type: "blocked", Priority: protocol.PriorityPriority})
		close(done)
	}()

	select {
	case <-done:
		t.Fatalf("expected control sender to block before reset")
	case <-time.After(50 * time.Millisecond):
	}

	tun.resetRuntimeState()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("blocked control sender did not exit after reset")
	}

	select {
	case msg := <-tun.priorityControlQ:
		t.Fatalf("unexpected stale priority control message after reset: %s", msg.Type)
	default:
	}
}

func TestTryDequeueControlDropsStaleQueuedMessageAfterReset(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtimeCtx, runtimeCancel := context.WithCancel(ctx)
	defer runtimeCancel()

	tun := &Tunnel{
		ctx:              ctx,
		priorityControlQ: make(chan *protocol.Message, 2),
		normalControlQ:   make(chan *protocol.Message, 1),
		outputQueues:     make(map[string]*sessionOutputQueue),
		outputReady:      make(chan struct{}, 1),
		runtimeCtx:       runtimeCtx,
		runtimeCancel:    runtimeCancel,
	}
	tun.runtimeGeneration.Store(1)

	stale := &protocol.Message{Type: "stale", Priority: protocol.PriorityPriority, Generation: 1}
	tun.priorityControlQ <- stale
	tun.resetRuntimeState()
	tun.priorityControlQ <- &protocol.Message{Type: "fresh", Priority: protocol.PriorityPriority, Generation: tun.currentRuntimeGeneration()}

	msg, ok := tun.tryDequeueControl(protocol.PriorityPriority)
	if !ok {
		t.Fatalf("expected fresh priority control message after stale one was dropped")
	}
	if msg.Type != "fresh" {
		t.Fatalf("expected stale control to be discarded, got %s", msg.Type)
	}
	select {
	case leftover := <-tun.priorityControlQ:
		t.Fatalf("expected stale priority control to be drained, found %s", leftover.Type)
	default:
	}
}

func TestWriteMessageOnConnRejectsStaleConnectionAfterSwap(t *testing.T) {
	tun := &Tunnel{conn: &websocket.Conn{}}
	staleConn := &websocket.Conn{}

	err := tun.writeMessageOnConn(staleConn, &protocol.Message{Type: "stale"})
	if err == nil || !strings.Contains(err.Error(), "stale connection") {
		t.Fatalf("expected stale connection error, got %v", err)
	}
}

func TestResetRuntimeStateRejectsStaleProcSpawn(t *testing.T) {
	tun := NewNode("https://example.com", NodeOptions{DataPath: t.TempDir()})
	oldProcs := tun.procs
	if oldProcs == nil {
		t.Fatalf("expected proc multiplexer to be initialized")
	}

	tun.resetRuntimeState()

	err := oldProcs.Spawn("stale-proc", []string{"bash", "-lc", "printf stale"}, "", nil, false, 0, 0, protocol.PriorityNormal)
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("expected old proc multiplexer to reject stale spawn after reset, got %v", err)
	}
	if oldProcs.Count() != 0 {
		t.Fatalf("expected retired proc multiplexer to stay empty, got %d session(s)", oldProcs.Count())
	}
	if tun.procs == nil {
		t.Fatalf("expected fresh proc multiplexer after reset")
	}
	if tun.procs.Count() != 0 {
		t.Fatalf("expected stale proc spawn not to touch the fresh runtime")
	}

	select {
	case msg := <-tun.priorityControlQ:
		t.Fatalf("unexpected stale priority control message: %s", msg.Type)
	default:
	}
	select {
	case msg := <-tun.normalControlQ:
		t.Fatalf("unexpected stale normal control message: %s", msg.Type)
	default:
	}
	if msg, ok := tun.dequeueOutputForPriority(protocol.PriorityPriority); ok {
		t.Fatalf("unexpected stale priority output message: %s", msg.Type)
	}
	if msg, ok := tun.dequeueOutputForPriority(protocol.PriorityNormal); ok {
		t.Fatalf("unexpected stale normal output message: %s", msg.Type)
	}
}

func TestResetRuntimeStateRetiresOldProcMuxBeforeKillAll(t *testing.T) {
	tun := NewNode("https://example.com", NodeOptions{DataPath: t.TempDir()})
	oldProcs := tun.procs
	if oldProcs == nil {
		t.Fatalf("expected proc multiplexer to be initialized")
	}

	done := beginBlockedRuntimeReset(t, tun)
	err := oldProcs.Spawn("stale-proc-race", []string{"bash", "-lc", "sleep 30"}, "", nil, false, 0, 0, protocol.PriorityNormal)
	finishBlockedRuntimeReset(t, tun, done)

	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("expected old proc multiplexer to reject stale spawn during reset, got %v", err)
	}
	if oldProcs.Count() != 0 {
		t.Fatalf("expected retired proc multiplexer to stay empty, got %d session(s)", oldProcs.Count())
	}
}

func TestProcExitIsQueuedAfterFinalOutput(t *testing.T) {
	tun := NewNode("https://example.com", NodeOptions{DataPath: t.TempDir()})
	if err := tun.procs.Spawn("ordered-proc-exit", []string{"bash", "-lc", "printf final"}, "", nil, false, 0, 0, protocol.PriorityNormal); err != nil {
		t.Fatalf("spawn proc session: %v", err)
	}

	waitForCondition(t, time.Second, func() bool {
		return tun.procs.Count() == 0
	})

	first, ok := tun.tryDequeueNextMessage(0)
	if !ok {
		t.Fatalf("expected queued proc.output message")
	}
	if first.Type != protocol.TypeProcOutput {
		t.Fatalf("expected proc.output before proc.exit, got %s", first.Type)
	}
	second, ok := tun.tryDequeueNextMessage(0)
	if !ok {
		t.Fatalf("expected queued proc.exit message")
	}
	if second.Type != protocol.TypeProcExit {
		t.Fatalf("expected proc.exit after proc.output, got %s", second.Type)
	}
}

func TestHandleProcStdinEOFClosesSession(t *testing.T) {
	tun := NewNode("https://example.com", NodeOptions{DataPath: t.TempDir()})
	generation := tun.currentRuntimeGeneration()
	if err := tun.procs.Spawn("stdin-eof", []string{"bash", "-lc", "cat"}, "", nil, false, 0, 0, protocol.PriorityNormal); err != nil {
		t.Fatalf("spawn proc session: %v", err)
	}

	payload, err := json.Marshal(protocol.ProcStdinPayload{
		SessionID: "stdin-eof",
		Data:      base64.StdEncoding.EncodeToString([]byte("hello")),
		EOF:       true,
	})
	if err != nil {
		t.Fatalf("marshal stdin payload: %v", err)
	}
	resp := tun.handler.Handle(&protocol.Message{Type: protocol.TypeProcStdin, Payload: payload, Generation: generation})
	if resp == nil || resp.Type != protocol.TypeProcStdinResult || resp.Error != nil {
		t.Fatalf("expected successful proc.stdin.result response, got %#v", resp)
	}

	waitForCondition(t, time.Second, func() bool {
		return tun.procs.Count() == 0
	})

	first, ok := tun.tryDequeueNextMessage(0)
	if !ok {
		t.Fatalf("expected queued proc.output message after stdin EOF")
	}
	if first.Type != protocol.TypeProcOutput {
		t.Fatalf("expected proc.output before proc.exit after stdin EOF, got %s", first.Type)
	}
	var output protocol.ProcOutputPayload
	if err := json.Unmarshal(first.Payload, &output); err != nil {
		t.Fatalf("unmarshal proc.output payload: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(output.Data)
	if err != nil {
		t.Fatalf("decode proc.output data: %v", err)
	}
	if string(decoded) != "hello" {
		t.Fatalf("expected proc.output payload %q after stdin EOF, got %q", "hello", string(decoded))
	}

	second, ok := tun.tryDequeueNextMessage(0)
	if !ok {
		t.Fatalf("expected queued proc.exit message after stdin EOF")
	}
	if second.Type != protocol.TypeProcExit {
		t.Fatalf("expected proc.exit after proc.output after stdin EOF, got %s", second.Type)
	}
	var exit protocol.ProcExitPayload
	if err := json.Unmarshal(second.Payload, &exit); err != nil {
		t.Fatalf("unmarshal proc.exit payload: %v", err)
	}
	if exit.ExitCode != 0 {
		t.Fatalf("expected exit code 0 after stdin EOF, got %d", exit.ExitCode)
	}
}

func TestHandleProcSpawnRejectsSessionReuseWhileOldOutputIsStillQueued(t *testing.T) {
	tun := NewNode("https://example.com", NodeOptions{DataPath: t.TempDir()})
	generation := tun.currentRuntimeGeneration()
	payload, err := json.Marshal(protocol.ProcSpawnPayload{
		SessionID: "reused-session",
		Argv:      []string{"bash", "-lc", "printf first"},
	})
	if err != nil {
		t.Fatalf("marshal spawn payload: %v", err)
	}

	if _, _, err := tun.handleProcSpawn(&protocol.Message{Type: protocol.TypeProcSpawn, Payload: payload, Generation: generation}); err != nil {
		t.Fatalf("first handleProcSpawn returned error: %v", err)
	}
	waitForCondition(t, time.Second, func() bool {
		return tun.procs.Count() == 0 && tun.outputSessionStateExists(outputSessionNamespaceProc, "reused-session")
	})

	if _, _, err := tun.handleProcSpawn(&protocol.Message{Type: protocol.TypeProcSpawn, Payload: payload, Generation: generation}); err == nil || !strings.Contains(err.Error(), "queued output") {
		t.Fatalf("expected session reuse with queued output to be rejected, got %v", err)
	}

	for i := 0; i < 4 && tun.outputSessionStateExists(outputSessionNamespaceProc, "reused-session"); i++ {
		if _, ok := tun.tryDequeueNextMessage(0); !ok {
			t.Fatalf("expected queued proc messages while draining reused-session")
		}
	}
	waitForCondition(t, time.Second, func() bool {
		return !tun.outputSessionStateExists(outputSessionNamespaceProc, "reused-session")
	})
	if _, _, err := tun.handleProcSpawn(&protocol.Message{Type: protocol.TypeProcSpawn, Payload: payload, Generation: generation}); err != nil {
		t.Fatalf("expected session_id reuse after drain, got %v", err)
	}
	waitForCondition(t, time.Second, func() bool {
		return tun.procs.Count() == 0
	})
}

func TestResetRuntimeStateRejectsStaleShellCreate(t *testing.T) {
	tun := New(&config.Config{Server: "https://example.com"}, "")
	oldShells := tun.shells
	if oldShells == nil {
		t.Fatalf("expected shell multiplexer to be initialized")
	}

	tun.resetRuntimeState()

	_, err := oldShells.Create("stale-shell", "bash", []string{"-lc", "exit 0"}, "", nil)
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("expected old shell multiplexer to reject stale create after reset, got %v", err)
	}
	if oldShells.Count() != 0 {
		t.Fatalf("expected retired shell multiplexer to stay empty, got %d session(s)", oldShells.Count())
	}
	if tun.shells == nil {
		t.Fatalf("expected fresh shell multiplexer after reset")
	}
	if tun.shells.Count() != 0 {
		t.Fatalf("expected stale shell create not to touch the fresh runtime")
	}
}

func TestResetRuntimeStateRetiresOldShellMuxBeforeKillAll(t *testing.T) {
	tun := New(&config.Config{Server: "https://example.com"}, "")
	oldShells := tun.shells
	if oldShells == nil {
		t.Fatalf("expected shell multiplexer to be initialized")
	}

	done := beginBlockedRuntimeReset(t, tun)
	_, err := oldShells.Create("stale-shell-race", "bash", []string{"-lc", "sleep 30"}, "", nil)
	finishBlockedRuntimeReset(t, tun, done)

	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("expected old shell multiplexer to reject stale create during reset, got %v", err)
	}
	if oldShells.Count() != 0 {
		t.Fatalf("expected retired shell multiplexer to stay empty, got %d session(s)", oldShells.Count())
	}
}

func TestHandleShellCreateQueuesExitAfterOutputAndRejectsReuseUntilDrain(t *testing.T) {
	tun := New(&config.Config{Server: "https://example.com"}, "")
	generation := tun.currentRuntimeGeneration()
	payload, err := json.Marshal(protocol.ShellCreatePayload{
		SessionID: "reused-shell-session",
		Shell:     "bash",
		Args:      []string{"-lc", "printf shell-output"},
	})
	if err != nil {
		t.Fatalf("marshal shell payload: %v", err)
	}

	if _, _, err := tun.handleShellCreate(&protocol.Message{Type: protocol.TypeShellCreate, Payload: payload, Generation: generation}); err != nil {
		t.Fatalf("first handleShellCreate returned error: %v", err)
	}
	waitForCondition(t, time.Second, func() bool {
		return tun.shells.Count() == 0 && tun.outputSessionStateExists(outputSessionNamespaceShell, "reused-shell-session")
	})

	if _, _, err := tun.handleShellCreate(&protocol.Message{Type: protocol.TypeShellCreate, Payload: payload, Generation: generation}); err == nil || !strings.Contains(err.Error(), "queued output") {
		t.Fatalf("expected shell session reuse with queued output to be rejected, got %v", err)
	}

	first, ok := tun.tryDequeueNextMessage(0)
	if !ok {
		t.Fatalf("expected queued shell.output message")
	}
	if first.Type != protocol.TypeShellOutput {
		t.Fatalf("expected shell.output before shell.exit, got %s", first.Type)
	}
	second, ok := tun.tryDequeueNextMessage(0)
	if !ok {
		t.Fatalf("expected queued shell.exit message")
	}
	if second.Type != protocol.TypeShellExit {
		t.Fatalf("expected shell.exit after shell.output, got %s", second.Type)
	}

	waitForCondition(t, time.Second, func() bool {
		return !tun.outputSessionStateExists(outputSessionNamespaceShell, "reused-shell-session")
	})
	if _, _, err := tun.handleShellCreate(&protocol.Message{Type: protocol.TypeShellCreate, Payload: payload, Generation: generation}); err != nil {
		t.Fatalf("expected shell session_id reuse after drain, got %v", err)
	}
	waitForCondition(t, time.Second, func() bool {
		return tun.shells.Count() == 0
	})
}

func TestHandleProcRunCanceledResponseKeepsPartialResultPayload(t *testing.T) {
	tun := NewNode("https://example.com", NodeOptions{DataPath: t.TempDir()})
	generation := tun.currentRuntimeGeneration()
	readyFile := filepath.Join(t.TempDir(), "ready")
	const runID = "cancel-payload-run"
	runPayload, err := json.Marshal(protocol.ProcRunPayload{
		RunID: runID,
		Argv:  []string{"bash", "-lc", "printf partial; printf ready > \"$READYFILE\"; sleep 10"},
		Env:   map[string]string{"READYFILE": readyFile},
	})
	if err != nil {
		t.Fatalf("marshal run payload: %v", err)
	}

	done := make(chan *protocol.Message, 1)
	go func() {
		done <- tun.handler.Handle(&protocol.Message{
			ID:         "transport-message-id",
			Type:       protocol.TypeProcRun,
			Payload:    runPayload,
			Generation: generation,
		})
	}()

	waitForCondition(t, time.Second, func() bool {
		_, err := os.Stat(readyFile)
		return err == nil
	})
	cancelPayload, err := json.Marshal(protocol.ProcCancelPayload{RunID: runID})
	if err != nil {
		t.Fatalf("marshal cancel payload: %v", err)
	}
	cancelResp := tun.handler.Handle(&protocol.Message{Type: protocol.TypeProcCancel, Payload: cancelPayload, Generation: generation})
	if cancelResp == nil || cancelResp.Error != nil {
		t.Fatalf("expected successful proc.cancel response, got %#v", cancelResp)
	}

	var runResp *protocol.Message
	select {
	case runResp = <-done:
	case <-time.After(time.Second):
		t.Fatalf("handler proc.run did not exit after proc.cancel")
	}
	if runResp == nil {
		t.Fatalf("expected proc.run handler response")
	}
	if runResp.Error != nil {
		t.Fatalf("expected canceled proc.run to return payload instead of rpc error, got %q", *runResp.Error)
	}
	if runResp.Type != protocol.TypeProcRunResult {
		t.Fatalf("expected proc.run.result response type, got %s", runResp.Type)
	}
	var result protocol.ProcRunResultPayload
	if err := json.Unmarshal(runResp.Payload, &result); err != nil {
		t.Fatalf("unmarshal proc.run response: %v", err)
	}
	if !result.Canceled {
		t.Fatalf("expected canceled flag in proc.run result payload")
	}
	if result.Stdout != "partial" {
		t.Fatalf("expected partial stdout to survive cancellation, got %q", result.Stdout)
	}
	if result.StdoutBase64 != base64.StdEncoding.EncodeToString([]byte("partial")) {
		t.Fatalf("unexpected stdout_b64 after cancellation: %q", result.StdoutBase64)
	}
	if result.TimedOut {
		t.Fatalf("expected explicit cancel not to report timeout")
	}
}

func TestHandleProcRunCancelsOnRuntimeReset(t *testing.T) {
	tun := NewNode("https://example.com", NodeOptions{DataPath: t.TempDir()})
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	payload, err := json.Marshal(protocol.ProcRunPayload{
		Argv: []string{"bash", "-lc", "sleep 10 & child=$!; printf '%s' \"$child\" > \"$PIDFILE\"; wait"},
		Env:  map[string]string{"PIDFILE": pidFile},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	done := make(chan struct{})
	var runErr error
	go func() {
		_, _, runErr = tun.handleProcRun(&protocol.Message{Type: protocol.TypeProcRun, Payload: payload})
		close(done)
	}()

	pid := waitForPIDFile(t, pidFile)
	tun.resetRuntimeState()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("handleProcRun did not exit after runtime reset")
	}
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("expected context cancellation error after runtime reset, got %v", runErr)
	}
	waitForCondition(t, time.Second, func() bool {
		return !pidAlive(pid)
	})
}

func TestHandleProcRunRejectsDuplicateInflightRunID(t *testing.T) {
	tun := NewNode("https://example.com", NodeOptions{DataPath: t.TempDir()})
	generation := tun.currentRuntimeGeneration()
	readyFile := filepath.Join(t.TempDir(), "ready")
	const runID = "duplicate-run-id"
	runPayload, err := json.Marshal(protocol.ProcRunPayload{
		RunID: runID,
		Argv:  []string{"bash", "-lc", "printf ready > \"$READYFILE\"; sleep 10"},
		Env:   map[string]string{"READYFILE": readyFile},
	})
	if err != nil {
		t.Fatalf("marshal run payload: %v", err)
	}

	done := make(chan struct{})
	var runErr error
	go func() {
		_, _, runErr = tun.handleProcRun(&protocol.Message{
			ID:         "first-run-request",
			Type:       protocol.TypeProcRun,
			Payload:    runPayload,
			Generation: generation,
		})
		close(done)
	}()

	waitForCondition(t, time.Second, func() bool {
		_, err := os.Stat(readyFile)
		return err == nil
	})
	_, _, err = tun.handleProcRun(&protocol.Message{
		ID:         "second-run-request",
		Type:       protocol.TypeProcRun,
		Payload:    runPayload,
		Generation: generation,
	})
	if err == nil || !strings.Contains(err.Error(), "already active") {
		t.Fatalf("expected duplicate in-flight run_id to be rejected, got %v", err)
	}

	cancelPayload, err := json.Marshal(protocol.ProcCancelPayload{RunID: runID})
	if err != nil {
		t.Fatalf("marshal cancel payload: %v", err)
	}
	if _, _, err := tun.handleProcCancel(&protocol.Message{Type: protocol.TypeProcCancel, Payload: cancelPayload, Generation: generation}); err != nil {
		t.Fatalf("handleProcCancel returned error: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("first proc.run did not exit after proc.cancel")
	}
	if runErr != nil {
		t.Fatalf("expected first proc.run to resolve with a normal canceled result, got %v", runErr)
	}
}

func TestHandleProcCancelCancelsSpecificRun(t *testing.T) {
	tun := NewNode("https://example.com", NodeOptions{DataPath: t.TempDir()})
	generation := tun.currentRuntimeGeneration()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	const runID = "relay-run-1"
	runPayload, err := json.Marshal(protocol.ProcRunPayload{
		RunID: runID,
		Argv:  []string{"bash", "-lc", "sleep 10 & child=$!; printf '%s' \"$child\" > \"$PIDFILE\"; wait"},
		Env:   map[string]string{"PIDFILE": pidFile},
	})
	if err != nil {
		t.Fatalf("marshal run payload: %v", err)
	}

	done := make(chan struct{})
	var runErr error
	var runRaw any
	go func() {
		_, runRaw, runErr = tun.handleProcRun(&protocol.Message{
			ID:         "transport-message-id",
			Type:       protocol.TypeProcRun,
			Payload:    runPayload,
			Generation: generation,
		})
		close(done)
	}()

	pid := waitForPIDFile(t, pidFile)
	cancelPayload, err := json.Marshal(protocol.ProcCancelPayload{RunID: runID})
	if err != nil {
		t.Fatalf("marshal cancel payload: %v", err)
	}
	_, raw, err := tun.handleProcCancel(&protocol.Message{
		Type:       protocol.TypeProcCancel,
		Payload:    cancelPayload,
		Generation: generation,
	})
	if err != nil {
		t.Fatalf("handleProcCancel returned error: %v", err)
	}
	result, ok := raw.(map[string]string)
	if !ok {
		t.Fatalf("expected proc.cancel result map, got %T", raw)
	}
	if result["status"] != "canceled" {
		t.Fatalf("expected proc.cancel to report canceled, got %v", result)
	}
	if result["run_id"] != runID {
		t.Fatalf("expected proc.cancel to echo run_id %q, got %v", runID, result)
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("handleProcRun did not exit after proc.cancel")
	}
	if runErr != nil {
		t.Fatalf("expected proc.cancel to resolve proc.run with a normal result, got %v", runErr)
	}
	resultPayload, ok := runRaw.(*protocol.ProcRunResultPayload)
	if !ok {
		t.Fatalf("expected canceled proc.run result payload, got %T", runRaw)
	}
	if !resultPayload.Canceled {
		t.Fatalf("expected direct proc.run result payload to mark cancellation")
	}
	waitForCondition(t, time.Second, func() bool {
		return !pidAlive(pid)
	})
}

func TestProcControlHandlersReturnSuccessAck(t *testing.T) {
	tun := NewNode("https://example.com", NodeOptions{DataPath: t.TempDir()})
	generation := tun.currentRuntimeGeneration()

	assertOKResponse := func(t *testing.T, resp *protocol.Message, wantType string) {
		t.Helper()
		if resp == nil {
			t.Fatalf("expected %s response", wantType)
		}
		if resp.Type != wantType {
			t.Fatalf("expected response type %s, got %s", wantType, resp.Type)
		}
		if resp.Error != nil {
			t.Fatalf("expected %s success response, got error %q", wantType, *resp.Error)
		}
		var result map[string]string
		if err := json.Unmarshal(resp.Payload, &result); err != nil {
			t.Fatalf("unmarshal %s payload: %v", wantType, err)
		}
		if result["status"] != "ok" {
			t.Fatalf("expected %s payload to report ok, got %v", wantType, result)
		}
	}

	t.Run("stdin", func(t *testing.T) {
		if err := tun.procs.Spawn("stdin-ack", []string{"bash", "-lc", "read line"}, "", nil, false, 0, 0, protocol.PriorityNormal); err != nil {
			t.Fatalf("spawn stdin session: %v", err)
		}
		payload, err := json.Marshal(protocol.ProcStdinPayload{SessionID: "stdin-ack", Data: base64.StdEncoding.EncodeToString([]byte("hello\n"))})
		if err != nil {
			t.Fatalf("marshal stdin payload: %v", err)
		}
		assertOKResponse(t, tun.handler.Handle(&protocol.Message{Type: protocol.TypeProcStdin, Payload: payload, Generation: generation}), protocol.TypeProcStdinResult)
	})

	t.Run("resize", func(t *testing.T) {
		if err := tun.procs.Spawn("resize-ack", []string{"bash", "-lc", "sleep 10"}, "", nil, true, 80, 24, protocol.PriorityNormal); err != nil {
			t.Fatalf("spawn resize session: %v", err)
		}
		payload, err := json.Marshal(protocol.ProcResizePayload{SessionID: "resize-ack", Cols: 100, Rows: 30})
		if err != nil {
			t.Fatalf("marshal resize payload: %v", err)
		}
		assertOKResponse(t, tun.handler.Handle(&protocol.Message{Type: protocol.TypeProcResize, Payload: payload, Generation: generation}), protocol.TypeProcResizeResult)
		_ = tun.procs.Kill("resize-ack")
	})

	t.Run("signal", func(t *testing.T) {
		if err := tun.procs.Spawn("signal-ack", []string{"bash", "-lc", "sleep 10"}, "", nil, false, 0, 0, protocol.PriorityNormal); err != nil {
			t.Fatalf("spawn signal session: %v", err)
		}
		payload, err := json.Marshal(protocol.ProcSignalPayload{SessionID: "signal-ack", Signal: "TERM"})
		if err != nil {
			t.Fatalf("marshal signal payload: %v", err)
		}
		assertOKResponse(t, tun.handler.Handle(&protocol.Message{Type: protocol.TypeProcSignal, Payload: payload, Generation: generation}), protocol.TypeProcSignalResult)
	})

	t.Run("kill", func(t *testing.T) {
		if err := tun.procs.Spawn("kill-ack", []string{"bash", "-lc", "sleep 10"}, "", nil, false, 0, 0, protocol.PriorityNormal); err != nil {
			t.Fatalf("spawn kill session: %v", err)
		}
		payload, err := json.Marshal(protocol.ProcKillPayload{SessionID: "kill-ack"})
		if err != nil {
			t.Fatalf("marshal kill payload: %v", err)
		}
		assertOKResponse(t, tun.handler.Handle(&protocol.Message{Type: protocol.TypeProcKill, Payload: payload, Generation: generation}), protocol.TypeProcKillResult)
	})
}

func TestHandleProcCancelFallsBackToRequestID(t *testing.T) {
	tun := NewNode("https://example.com", NodeOptions{DataPath: t.TempDir()})
	generation := tun.currentRuntimeGeneration()
	canceled := false
	if err := tun.rememberProcRunCancel("legacy-request-id", generation, func(error) {
		canceled = true
	}); err != nil {
		t.Fatalf("rememberProcRunCancel returned error: %v", err)
	}

	cancelPayload, err := json.Marshal(protocol.ProcCancelPayload{RunID: "fabricated-run-id", RequestID: "legacy-request-id"})
	if err != nil {
		t.Fatalf("marshal cancel payload: %v", err)
	}
	_, raw, err := tun.handleProcCancel(&protocol.Message{
		Type:       protocol.TypeProcCancel,
		Payload:    cancelPayload,
		Generation: generation,
	})
	if err != nil {
		t.Fatalf("handleProcCancel returned error: %v", err)
	}
	if !canceled {
		t.Fatalf("expected legacy request_id cancel handle to remain supported")
	}
	result, ok := raw.(map[string]string)
	if !ok {
		t.Fatalf("expected proc.cancel result map, got %T", raw)
	}
	if result["status"] != "canceled" || result["request_id"] != "legacy-request-id" {
		t.Fatalf("unexpected proc.cancel result for legacy request_id: %v", result)
	}
	if _, ok := result["run_id"]; ok {
		t.Fatalf("expected legacy request_id cancel not to fabricate run_id, got %v", result)
	}
}

func TestHandleFileWriteRejectsStaleGenerationAfterReset(t *testing.T) {
	tun := NewNode("https://example.com", NodeOptions{DataPath: t.TempDir()})
	generation := tun.currentRuntimeGeneration()
	targetPath := filepath.Join(t.TempDir(), "stale.txt")
	payload, err := json.Marshal(protocol.FileWritePayload{
		Path:    targetPath,
		Content: base64.StdEncoding.EncodeToString([]byte("stale")),
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	tun.resetRuntimeState()

	_, _, err = tun.handleFileWrite(&protocol.Message{
		Type:       protocol.TypeFileWrite,
		Payload:    payload,
		Generation: generation,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected stale file.write to be canceled after reset, got %v", err)
	}
	if _, statErr := os.Stat(targetPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("expected stale file.write not to create %s, stat err=%v", targetPath, statErr)
	}
}

func TestHandleFileGrepCancelsOnRuntimeResetAfterStart(t *testing.T) {
	pidPath := installBlockingRipgrep(t)
	tun := NewNode("https://example.com", NodeOptions{DataPath: t.TempDir()})
	targetDir := t.TempDir()
	payload, err := json.Marshal(protocol.FileGrepPayload{
		Path:    targetDir,
		Pattern: "stale",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	done := make(chan struct{})
	var grepErr error
	go func() {
		_, _, grepErr = tun.handleFileGrep(&protocol.Message{
			Type:       protocol.TypeFileGrep,
			Payload:    payload,
			Generation: tun.currentRuntimeGeneration(),
		})
		close(done)
	}()

	pid := waitForPIDFile(t, pidPath)
	tun.resetRuntimeState()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("handleFileGrep did not exit after runtime reset")
	}
	if !errors.Is(grepErr, context.Canceled) {
		t.Fatalf("expected context cancellation error after runtime reset, got %v", grepErr)
	}
	waitForCondition(t, time.Second, func() bool {
		return !pidAlive(pid)
	})
}

func TestHandleProcSpawnRejectsStaleGenerationAfterReset(t *testing.T) {
	tun := NewNode("https://example.com", NodeOptions{DataPath: t.TempDir()})
	generation := tun.currentRuntimeGeneration()
	payload, err := json.Marshal(protocol.ProcSpawnPayload{
		SessionID: "stale-spawn",
		Argv:      []string{"bash", "-lc", "sleep 10"},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	tun.resetRuntimeState()

	_, _, err = tun.handleProcSpawn(&protocol.Message{
		Type:       protocol.TypeProcSpawn,
		Payload:    payload,
		Generation: generation,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected stale proc.spawn to be canceled after reset, got %v", err)
	}
	if tun.procs == nil {
		t.Fatalf("expected fresh proc multiplexer after reset")
	}
	if tun.procs.Count() != 0 {
		t.Fatalf("expected stale proc.spawn not to touch the fresh runtime")
	}
}

func TestHandleProcRunResultUsesBase64ForBinaryOutput(t *testing.T) {
	tun := NewNode("https://example.com", NodeOptions{DataPath: t.TempDir()})
	payload, err := json.Marshal(protocol.ProcRunPayload{
		Argv: []string{"bash", "-lc", "printf '\\377\\376'; printf '\\377' >&2"},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	_, raw, err := tun.handleProcRun(&protocol.Message{Type: protocol.TypeProcRun, Payload: payload})
	if err != nil {
		t.Fatalf("handleProcRun returned error: %v", err)
	}
	result, ok := raw.(*protocol.ProcRunResultPayload)
	if !ok {
		t.Fatalf("expected ProcRunResultPayload, got %T", raw)
	}
	if result.Stdout != "" {
		t.Fatalf("expected binary stdout text field to be omitted, got %q", result.Stdout)
	}
	if result.Stderr != "" {
		t.Fatalf("expected binary stderr text field to be omitted, got %q", result.Stderr)
	}
	if result.StdoutBase64 != base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe}) {
		t.Fatalf("unexpected stdout_b64: %q", result.StdoutBase64)
	}
	if result.StderrBase64 != base64.StdEncoding.EncodeToString([]byte{0xff}) {
		t.Fatalf("unexpected stderr_b64: %q", result.StderrBase64)
	}
}
