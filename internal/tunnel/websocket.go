package tunnel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"github.com/taurusagents/taurus-relay/internal/auth"
	"github.com/taurusagents/taurus-relay/internal/config"
	"github.com/taurusagents/taurus-relay/internal/docker"
	"github.com/taurusagents/taurus-relay/internal/fileops"
	"github.com/taurusagents/taurus-relay/internal/health"
	"github.com/taurusagents/taurus-relay/internal/proc"
	"github.com/taurusagents/taurus-relay/internal/protocol"
	"github.com/taurusagents/taurus-relay/internal/shell"
)

type Mode string

const (
	ModeConnect Mode = "connect"
	ModeNode    Mode = "node"
)

const (
	controlQueueSize           = 256
	sessionQueueMaxMessages    = 128
	sessionQueueMaxBytes       = 1024 * 1024 // 1 MiB per session
	containerExecReapTimeoutMs = 3000
	priorityBurstLimit         = 8 // let normal traffic through after a short priority burst
)

type queuedOutput struct {
	msg  *protocol.Message
	size int
}

type sessionOutputQueue struct {
	items          []queuedOutput
	bytes          int
	full           *sync.Cond
	active         bool
	closed         bool
	drainWhenEmpty bool
	writers        int
	priority       string
}

type NodeOptions struct {
	Name          string
	Host          string
	Token         string
	DataPath      string
	MaxContainers int
}

// runtimeSnapshot captures the runtime-bound objects a request is allowed to
// use. Requests read from an older websocket generation keep using this
// snapshot so they cannot accidentally hop onto the fresh runtime created after
// a reconnect/reset.
type runtimeSnapshot struct {
	ctx    context.Context
	shells *shell.Multiplexer
	execs  *docker.ExecMultiplexer
	procs  *proc.Multiplexer
}

type procRunCancelHandleKey struct {
	generation uint64
	cancelID   string
}

type procRunCancelEntry struct {
	cancel func(error)
}

var procRunCanceledCause = errors.New("proc.run canceled")

// Tunnel manages the WebSocket connection and message routing.
type Tunnel struct {
	cfg       *config.Config
	token     string // one-time registration token (empty if already registered)
	reconnCfg ReconnectConfig
	mode      Mode
	node      *NodeOptions

	conn    *websocket.Conn
	connMu  sync.Mutex
	handler *protocol.Handler

	shells *shell.Multiplexer
	execs  *docker.ExecMultiplexer
	procs  *proc.Multiplexer
	docker *docker.Client

	priorityControlQ chan *protocol.Message
	normalControlQ   chan *protocol.Message

	outputMu                sync.Mutex
	outputQueues            map[string]*sessionOutputQueue
	completedOutputSessions map[string]struct{}
	activePriorityOutputs   []string
	activeNormalOutputs     []string
	priorityRRIndex         int
	normalRRIndex           int
	outputReady             chan struct{}
	runtimeGeneration       atomic.Uint64
	runtimeMu               sync.RWMutex
	runtimeCtx              context.Context
	runtimeCancel           context.CancelFunc
	procRunMu               sync.Mutex
	procRunCancels          map[procRunCancelHandleKey]procRunCancelEntry

	stopCh chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
}

type outputSessionNamespace string

const (
	outputSessionNamespaceShell         outputSessionNamespace = "shell"
	outputSessionNamespaceProc          outputSessionNamespace = "proc"
	outputSessionNamespaceContainerExec outputSessionNamespace = "container.exec"
)

func outputSessionQueueKey(namespace outputSessionNamespace, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	// Queue state is shared across all streamed APIs in a tunnel. Scope it by API
	// family so proc/container.exec/shell can safely reuse the same bare
	// session_id without inheriting each other's buffered-output completion state.
	return string(namespace) + "\x00" + sessionID
}

// New creates a new Tunnel for user relay mode.
func New(cfg *config.Config, token string) *Tunnel {
	return newTunnel(cfg, token, ModeConnect, nil)
}

// NewNode creates a new Tunnel for node relay mode.
func NewNode(server string, opts NodeOptions) *Tunnel {
	cfg := &config.Config{Server: server}
	return newTunnel(cfg, opts.Token, ModeNode, &opts)
}

func newTunnel(cfg *config.Config, token string, mode Mode, nodeOpts *NodeOptions) *Tunnel {
	ctx, cancel := context.WithCancel(context.Background())
	runtimeCtx, runtimeCancel := context.WithCancel(ctx)

	t := &Tunnel{
		cfg:              cfg,
		token:            token,
		reconnCfg:        DefaultReconnectConfig(),
		mode:             mode,
		node:             nodeOpts,
		handler:          protocol.NewHandler(),
		priorityControlQ: make(chan *protocol.Message, controlQueueSize),
		normalControlQ:   make(chan *protocol.Message, controlQueueSize),

		outputQueues:            make(map[string]*sessionOutputQueue),
		completedOutputSessions: make(map[string]struct{}),
		outputReady:             make(chan struct{}, 1),
		procRunCancels:          make(map[procRunCancelHandleKey]procRunCancelEntry),

		stopCh: make(chan struct{}),
		ctx:    ctx,
		cancel: cancel,

		runtimeCtx:    runtimeCtx,
		runtimeCancel: runtimeCancel,
	}

	t.runtimeGeneration.Store(1)
	t.rebuildRuntimeMultiplexers(t.currentRuntimeGeneration())

	t.registerHandlers(mode)
	return t
}

func (t *Tunnel) currentRuntimeGeneration() uint64 {
	return t.runtimeGeneration.Load()
}

func (t *Tunnel) requestGeneration(msg *protocol.Message) uint64 {
	if msg != nil && msg.Generation != 0 {
		return msg.Generation
	}
	return t.currentRuntimeGeneration()
}

func (t *Tunnel) rememberProcRunCancel(cancelID string, generation uint64, cancel func(error)) error {
	if cancelID == "" || cancel == nil {
		return nil
	}
	key := procRunCancelHandleKey{generation: generation, cancelID: cancelID}
	t.procRunMu.Lock()
	defer t.procRunMu.Unlock()
	if _, exists := t.procRunCancels[key]; exists {
		return fmt.Errorf("proc run %q already active", cancelID)
	}
	t.procRunCancels[key] = procRunCancelEntry{cancel: cancel}
	return nil
}

func (t *Tunnel) forgetProcRunCancel(cancelID string, generation uint64) {
	if cancelID == "" {
		return
	}
	t.procRunMu.Lock()
	delete(t.procRunCancels, procRunCancelHandleKey{generation: generation, cancelID: cancelID})
	t.procRunMu.Unlock()
}

func (t *Tunnel) cancelProcRun(cancelID string, generation uint64) bool {
	if cancelID == "" {
		return false
	}
	key := procRunCancelHandleKey{generation: generation, cancelID: cancelID}
	t.procRunMu.Lock()
	entry, ok := t.procRunCancels[key]
	if ok {
		delete(t.procRunCancels, key)
	}
	t.procRunMu.Unlock()
	if !ok || entry.cancel == nil {
		return false
	}
	entry.cancel(procRunCanceledCause)
	return true
}

func procRunCancelID(runID, requestID string) string {
	if runID != "" {
		return runID
	}
	return requestID
}

func (t *Tunnel) markOutputSessionComplete(sessionKey string) {
	if sessionKey == "" {
		return
	}
	t.outputMu.Lock()
	t.completedOutputSessions[sessionKey] = struct{}{}
	if q := t.outputQueues[sessionKey]; q != nil {
		q.drainWhenEmpty = true
		t.maybeDeleteOutputQueueLocked(sessionKey, q)
	}
	t.outputMu.Unlock()
}

func (t *Tunnel) markOutputSessionCompleteFor(namespace outputSessionNamespace, sessionID string) {
	t.markOutputSessionComplete(outputSessionQueueKey(namespace, sessionID))
}

func (t *Tunnel) maybeDeleteOutputQueueLocked(sessionKey string, q *sessionOutputQueue) {
	if q == nil || q.active || len(q.items) != 0 || q.writers != 0 || !q.drainWhenEmpty {
		return
	}
	if t.outputQueues[sessionKey] == q {
		delete(t.outputQueues, sessionKey)
	}
	delete(t.completedOutputSessions, sessionKey)
}

func (t *Tunnel) outputSessionStateExists(namespace outputSessionNamespace, sessionID string) bool {
	sessionKey := outputSessionQueueKey(namespace, sessionID)
	if sessionKey == "" {
		return false
	}
	t.outputMu.Lock()
	defer t.outputMu.Unlock()
	if _, ok := t.outputQueues[sessionKey]; ok {
		return true
	}
	_, ok := t.completedOutputSessions[sessionKey]
	return ok
}

func procRunResultPayload(result *proc.RunResult, canceled bool) *protocol.ProcRunResultPayload {
	if result == nil {
		return nil
	}
	stdoutText := ""
	if utf8.ValidString(result.Stdout) {
		stdoutText = result.Stdout
	}
	stderrText := ""
	if utf8.ValidString(result.Stderr) {
		stderrText = result.Stderr
	}
	return &protocol.ProcRunResultPayload{
		Stdout:       stdoutText,
		StdoutBase64: base64.StdEncoding.EncodeToString([]byte(result.Stdout)),
		Stderr:       stderrText,
		StderrBase64: base64.StdEncoding.EncodeToString([]byte(result.Stderr)),
		ExitCode:     result.ExitCode,
		DurationMs:   result.Duration.Milliseconds(),
		TimedOut:     result.TimedOut,
		Canceled:     canceled,
	}
}

func (t *Tunnel) runtimeSnapshotForMessage(msg *protocol.Message) (runtimeSnapshot, error) {
	generation := uint64(0)
	if msg != nil {
		generation = msg.Generation
	}
	return t.runtimeSnapshotForGeneration(generation)
}

func (t *Tunnel) runtimeSnapshotForGeneration(generation uint64) (runtimeSnapshot, error) {
	t.runtimeMu.RLock()
	defer t.runtimeMu.RUnlock()
	if generation != 0 && generation != t.runtimeGeneration.Load() {
		return runtimeSnapshot{}, context.Canceled
	}
	return runtimeSnapshot{
		ctx:    t.runtimeCtx,
		shells: t.shells,
		execs:  t.execs,
		procs:  t.procs,
	}, nil
}

func (t *Tunnel) shellsForMessage(msg *protocol.Message) (*shell.Multiplexer, error) {
	runtime, err := t.runtimeSnapshotForMessage(msg)
	if err != nil {
		return nil, err
	}
	if runtime.shells == nil {
		return nil, fmt.Errorf("shell multiplexer not initialized")
	}
	return runtime.shells, nil
}

func (t *Tunnel) procsForMessage(msg *protocol.Message) (*proc.Multiplexer, error) {
	runtime, err := t.runtimeSnapshotForMessage(msg)
	if err != nil {
		return nil, err
	}
	if runtime.procs == nil {
		return nil, fmt.Errorf("proc multiplexer not initialized")
	}
	return runtime.procs, nil
}

func (t *Tunnel) execsForMessage(msg *protocol.Message) (*docker.ExecMultiplexer, error) {
	runtime, err := t.runtimeSnapshotForMessage(msg)
	if err != nil {
		return nil, err
	}
	if runtime.execs == nil {
		return nil, fmt.Errorf("exec multiplexer not initialized")
	}
	return runtime.execs, nil
}

func (t *Tunnel) rebuildRuntimeMultiplexers(generation uint64) {
	t.shells = nil
	t.execs = nil
	t.procs = nil

	if t.mode == ModeConnect {
		t.shells = shell.NewMultiplexer(
			func(sessionID string, data []byte) {
				payload, _ := json.Marshal(protocol.ShellOutputPayload{
					SessionID: sessionID,
					Data:      base64.StdEncoding.EncodeToString(data),
				})
				msg := &protocol.Message{Type: protocol.TypeShellOutput, Payload: payload}
				t.enqueueOutputForSessionForGeneration(generation, outputSessionNamespaceShell, sessionID, protocol.PriorityNormal, msg, len(payload))
			},
			func(sessionID string, exitCode int) {
				t.markOutputSessionCompleteFor(outputSessionNamespaceShell, sessionID)
				payload, _ := json.Marshal(protocol.ShellExitPayload{SessionID: sessionID, ExitCode: exitCode})
				t.enqueueOutputForSessionForGeneration(generation, outputSessionNamespaceShell, sessionID, protocol.PriorityNormal, &protocol.Message{Type: protocol.TypeShellExit, Payload: payload}, len(payload))
			},
		)
		return
	}

	if t.mode == ModeNode && t.node != nil {
		if t.docker == nil {
			t.docker = docker.NewClient(t.node.DataPath)
		}
		t.execs = docker.NewExecMultiplexer(
			func(sessionID string, data []byte) {
				payload, _ := json.Marshal(protocol.ShellOutputPayload{
					SessionID: sessionID,
					Data:      base64.StdEncoding.EncodeToString(data),
				})
				msg := &protocol.Message{Type: protocol.TypeContainerExecOutput, Payload: payload}
				t.enqueueOutputForSessionForGeneration(generation, outputSessionNamespaceContainerExec, sessionID, protocol.PriorityNormal, msg, len(payload))
			},
			func(sessionID string, exitCode int) {
				t.markOutputSessionCompleteFor(outputSessionNamespaceContainerExec, sessionID)
				payload, _ := json.Marshal(protocol.ShellExitPayload{SessionID: sessionID, ExitCode: exitCode})
				t.enqueueOutputForSessionForGeneration(generation, outputSessionNamespaceContainerExec, sessionID, protocol.PriorityNormal, &protocol.Message{Type: protocol.TypeContainerExecExit, Payload: payload}, len(payload))
			},
		)
		t.procs = proc.NewMultiplexer(
			func(sessionID, stream string, data []byte, priority string) {
				payload, _ := json.Marshal(protocol.ProcOutputPayload{
					SessionID: sessionID,
					Stream:    stream,
					Data:      base64.StdEncoding.EncodeToString(data),
				})
				msg := &protocol.Message{Type: protocol.TypeProcOutput, Payload: payload, Priority: priority}
				t.enqueueOutputForSessionForGeneration(generation, outputSessionNamespaceProc, sessionID, priority, msg, len(payload))
			},
			func(sessionID string, exitCode int, priority string) {
				t.markOutputSessionCompleteFor(outputSessionNamespaceProc, sessionID)
				payload, _ := json.Marshal(protocol.ProcExitPayload{SessionID: sessionID, ExitCode: exitCode})
				// Route proc.exit through the per-session output queue so the final
				// exit notification cannot overtake already-buffered proc.output.
				t.enqueueOutputForSessionForGeneration(generation, outputSessionNamespaceProc, sessionID, priority, &protocol.Message{Type: protocol.TypeProcExit, Payload: payload, Priority: priority}, len(payload))
			},
		)
	}
}

// registerHandlers sets up message handlers for all supported message types.
func (t *Tunnel) registerHandlers(mode Mode) {
	h := t.handler

	if mode == ModeConnect {
		h.Register(protocol.TypeShellCreate, t.handleShellCreate)
		h.Register(protocol.TypeShellExec, t.handleShellExec)
		h.Register(protocol.TypeShellKill, t.handleShellKill)
		h.Register(protocol.TypeShellWriteStdin, t.handleShellWriteStdin)
		h.Register(protocol.TypeShellResize, t.handleShellResize)
		h.Register(protocol.TypeShellSignal, t.handleShellSignal)
	}

	if mode == ModeNode {
		h.Register(protocol.TypeProcRun, t.handleProcRun)
		h.Register(protocol.TypeProcCancel, t.handleProcCancel)
		h.Register(protocol.TypeProcSpawn, t.handleProcSpawn)
		h.Register(protocol.TypeProcStdin, t.handleProcStdin)
		h.Register(protocol.TypeProcResize, t.handleProcResize)
		h.Register(protocol.TypeProcSignal, t.handleProcSignal)
		h.Register(protocol.TypeProcKill, t.handleProcKill)
		h.Register(protocol.TypeProcCheckAlive, t.handleProcCheckAlive)
		h.Register(protocol.TypeContainerEnsure, t.handleContainerEnsure)
		h.Register(protocol.TypeContainerExec, t.handleContainerExec)
		h.Register(protocol.TypeContainerExecStdin, t.handleContainerExecStdin)
		h.Register(protocol.TypeContainerExecResize, t.handleContainerExecResize)
		h.Register(protocol.TypeContainerExecSignal, t.handleContainerExecSignal)
		h.Register(protocol.TypeContainerExecKill, t.handleContainerExecKill)
		h.Register(protocol.TypeContainerExecCheckAlive, t.handleContainerExecCheckAlive)
		h.Register(protocol.TypeContainerPause, t.handleContainerPause)
		h.Register(protocol.TypeContainerUnpause, t.handleContainerUnpause)
		h.Register(protocol.TypeContainerStop, t.handleContainerStop)
		h.Register(protocol.TypeContainerDestroy, t.handleContainerDestroy)
		h.Register(protocol.TypeContainerStatus, t.handleContainerStatus)
		h.Register(protocol.TypeContainerExecCommand, t.handleContainerExecCommand)
		h.Register(protocol.TypeContainerExecWithStdin, t.handleContainerExecWithStdin)
	}

	h.Register(protocol.TypeFileRead, t.handleFileRead)
	h.Register(protocol.TypeFileWrite, t.handleFileWrite)
	h.Register(protocol.TypeFileStat, t.handleFileStat)
	h.Register(protocol.TypeFileGlob, t.handleFileGlob)
	h.Register(protocol.TypeFileGrep, t.handleFileGrep)
	h.Register(protocol.TypeFileMkdir, t.handleFileMkdir)
	h.Register(protocol.TypeFileRemove, t.handleFileRemove)

	h.Register(protocol.TypePing, func(_ *protocol.Message) (string, any, error) {
		return protocol.TypePong, map[string]string{"status": "ok"}, nil
	})
}

// Run connects and runs the tunnel loop with auto-reconnect.
func (t *Tunnel) Run() error {
	attempt := 0
	for {
		select {
		case <-t.ctx.Done():
			return nil
		default:
		}

		err := t.connect()
		if err != nil {
			if t.reconnCfg.MaxRetries > 0 && attempt >= t.reconnCfg.MaxRetries {
				return fmt.Errorf("max reconnect attempts reached: %w", err)
			}
			delay := t.reconnCfg.Backoff(attempt)
			log.Printf("[tunnel] connection failed: %v — retrying in %v", err, delay)
			select {
			case <-time.After(delay):
			case <-t.ctx.Done():
				return nil
			}
			attempt++
			continue
		}

		// Reset attempt counter on successful connection
		attempt = 0

		// Run the message loop
		err = t.messageLoop()
		if err != nil {
			log.Printf("[tunnel] connection lost: %v", err)
		}

		// Connection lost — reconnect
		t.connMu.Lock()
		if t.conn != nil {
			t.conn.Close()
			t.conn = nil
		}
		t.connMu.Unlock()
		t.resetRuntimeState()

		log.Printf("[tunnel] will reconnect...")
		select {
		case <-time.After(t.reconnCfg.InitialDelay):
		case <-t.ctx.Done():
			return nil
		}
	}
}

// Stop gracefully shuts down the tunnel.
func (t *Tunnel) Stop() {
	t.cancel()
	t.resetRuntimeState()
	t.connMu.Lock()
	if t.conn != nil {
		_ = t.conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "relay shutting down"))
		t.conn.Close()
	}
	t.connMu.Unlock()
}

func (t *Tunnel) resetRuntimeState() {
	// Swap to a fresh runtime generation before cancelling the previous runtime.
	// That way any stale request/control goroutine that wakes up on cancellation
	// sees the new generation and exits instead of leaking into the new runtime.
	nextCtx, nextCancel := context.WithCancel(t.ctx)

	t.runtimeMu.Lock()
	prevCancel := t.runtimeCancel
	oldShells := t.shells
	oldExecs := t.execs
	oldProcs := t.procs
	// Retire the old muxes at the reset boundary before we cancel and tear the
	// runtime down. Stale handlers can keep these pointers, so they must reject
	// new Create/Spawn calls immediately instead of waiting for KillAll().
	if oldShells != nil {
		oldShells.Close()
	}
	if oldExecs != nil {
		oldExecs.Close()
	}
	if oldProcs != nil {
		oldProcs.Close()
	}
	generation := t.runtimeGeneration.Add(1)
	t.runtimeCtx = nextCtx
	t.runtimeCancel = nextCancel
	t.rebuildRuntimeMultiplexers(generation)
	t.runtimeMu.Unlock()

	if prevCancel != nil {
		prevCancel()
	}

	t.outputMu.Lock()
	for _, q := range t.outputQueues {
		q.closed = true
		q.full.Broadcast()
	}
	t.outputQueues = make(map[string]*sessionOutputQueue)
	t.completedOutputSessions = make(map[string]struct{})
	t.activePriorityOutputs = nil
	t.activeNormalOutputs = nil
	t.priorityRRIndex = 0
	t.normalRRIndex = 0
	t.outputMu.Unlock()
	if oldShells != nil {
		oldShells.KillAll()
	}
	if oldExecs != nil {
		oldExecs.KillAll()
	}
	if oldProcs != nil {
		oldProcs.KillAll()
	}
	t.drainControlQueues()
}

func (t *Tunnel) drainControlQueues() {
	for {
		select {
		case <-t.priorityControlQ:
		default:
			goto drainNormal
		}
	}

drainNormal:
	for {
		select {
		case <-t.normalControlQ:
		default:
			return
		}
	}
}

// connect establishes the WebSocket connection and performs authentication.
func (t *Tunnel) connect() error {
	wsURL := t.wsURL()
	log.Printf("[tunnel] connecting to %s", wsURL)

	dialer := websocket.Dialer{
		EnableCompression: false,
	}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("websocket dial: %w", err)
	}

	t.connMu.Lock()
	t.conn = conn
	t.connMu.Unlock()

	log.Printf("[tunnel] connected")

	// Always authenticate via a message (server expects type: "auth" as first message)
	if err := t.authenticate(); err != nil {
		conn.Close()
		return fmt.Errorf("authentication: %w", err)
	}

	return nil
}

// authenticate sends the first auth message and waits for the corresponding result.
func (t *Tunnel) authenticate() error {
	if t.mode == ModeNode {
		return t.authenticateNode()
	}
	return t.authenticateConnect()
}

func (t *Tunnel) authenticateConnect() error {
	var msg *protocol.Message
	var err error

	if t.token != "" && t.cfg.HasCredentials() {
		log.Printf("[tunnel] ignoring --token (already registered as target %s). To register a new target, delete %s first.",
			t.cfg.TargetID, config.Path())
		msg, err = auth.BuildReconnectMessage(t.cfg.JWT)
		if err != nil {
			return err
		}
		log.Printf("[tunnel] authenticating with JWT (target %s)", t.cfg.TargetID)
	} else if t.token != "" {
		msg, err = auth.BuildRegistrationMessage(t.token)
		if err != nil {
			return err
		}
		log.Printf("[tunnel] registering with token")
	} else if t.cfg.HasCredentials() {
		msg, err = auth.BuildReconnectMessage(t.cfg.JWT)
		if err != nil {
			return err
		}
		log.Printf("[tunnel] authenticating with JWT (target %s)", t.cfg.TargetID)
	} else {
		return fmt.Errorf("no credentials or registration token available")
	}

	if err := t.writeMessage(msg); err != nil {
		return fmt.Errorf("send auth message: %w", err)
	}

	resp, err := t.readAuthResponse()
	if err != nil {
		return err
	}
	if resp.Type != protocol.TypeAuthResult {
		return fmt.Errorf("expected auth.result, got %s", resp.Type)
	}
	if err := auth.HandleAuthResult(resp, t.cfg); err != nil {
		return err
	}

	t.token = ""
	log.Printf("[tunnel] authenticated as target %s", t.cfg.TargetID)
	return nil
}

func (t *Tunnel) authenticateNode() error {
	if t.node == nil {
		return fmt.Errorf("node mode requested without node options")
	}
	ramGB, cpus := health.NodeAllocatable()
	hostname, _ := os.Hostname()
	sys := health.SysInfo(0)
	payload := protocol.NodeRegisterPayload{
		Type:             "node",
		Name:             t.node.Name,
		Host:             t.node.Host,
		EnrollmentToken:  t.token,
		AllocatableRAMGB: ramGB,
		AllocatableCPUs:  cpus,
		MaxContainers:    t.node.MaxContainers,
		Capabilities: map[string]bool{
			"docker":         true,
			"fs_read":        true,
			"exec_streaming": true,
		},
		Meta: buildNodeRegisterMeta(t.node.DataPath, sys, hostname),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal node.register payload: %w", err)
	}
	if err := t.writeMessage(&protocol.Message{ID: "node-register", Type: protocol.TypeNodeRegister, Payload: data}); err != nil {
		return fmt.Errorf("send node.register: %w", err)
	}

	resp, err := t.readAuthResponse()
	if err != nil {
		return err
	}
	if resp.Type != protocol.TypeNodeRegisterResult {
		return fmt.Errorf("expected node.register.result, got %s", resp.Type)
	}
	var result protocol.NodeRegisterResultPayload
	if err := json.Unmarshal(resp.Payload, &result); err != nil {
		return fmt.Errorf("parse node.register.result: %w", err)
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "node registration failed"
		}
		return fmt.Errorf(result.Error)
	}
	log.Printf("[tunnel] registered node %s", result.NodeID)
	return nil
}

// buildNodeRegisterMeta publishes both the operator-configured data root and the
// concrete Taurus drive root so the control plane can derive remote host paths
// without guessing which node layout it is talking to.
func buildNodeRegisterMeta(dataPath string, sys *protocol.HeartbeatPayload, hostname string) map[string]string {
	dataRoot := filepath.Clean(dataPath)
	drivePath := filepath.Join(dataRoot, "taurus-drives")

	return map[string]string{
		"os":                sys.OS,
		"arch":              sys.Arch,
		"hostname":          hostname,
		"data_root":         dataRoot,
		"drive_path":        drivePath,
		"taurus_drive_path": drivePath,
	}
}

func (t *Tunnel) readAuthResponse() (*protocol.Message, error) {
	_, data, err := t.conn.ReadMessage()
	if err != nil {
		return nil, fmt.Errorf("read auth response: %w", err)
	}
	var resp protocol.Message
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse auth response: %w", err)
	}
	return &resp, nil
}

func (t *Tunnel) heartbeatInfo() *protocol.HeartbeatPayload {
	if t.mode == ModeNode {
		sessions := 0
		if t.execs != nil {
			sessions += t.execs.Count()
		}
		if t.procs != nil {
			sessions += t.procs.Count()
		}
		containerCount := 0
		dataPath := "/"
		if t.node != nil {
			dataPath = t.node.DataPath
		}
		if t.docker != nil {
			containerCount = t.docker.RunningContainerCount()
		}
		return health.NodeSysInfo(sessions, dataPath, containerCount)
	}
	sessions := 0
	if t.shells != nil {
		sessions = t.shells.Count()
	}
	return health.SysInfo(sessions)
}

// messageLoop runs the main read/write loop.
func (t *Tunnel) messageLoop() error {
	generation := t.currentRuntimeGeneration()
	t.connMu.Lock()
	conn := t.conn
	t.connMu.Unlock()
	if conn == nil {
		return fmt.Errorf("not connected")
	}
	// Start heartbeat
	heartbeatStop := make(chan struct{})
	go health.HeartbeatLoop(30*time.Second, t.heartbeatInfo, func(msg *protocol.Message) {
		t.enqueueControlForGeneration(generation, msg)
	}, heartbeatStop)
	writerDone := make(chan struct{})
	defer func() {
		close(heartbeatStop)
		// Close the generation-bound socket and wait for the matching writer to exit
		// before Run() continues to the next reconnect attempt. That keeps an older
		// writer from surviving long enough to drain fresh queue entries.
		_ = conn.Close()
		<-writerDone
	}()

	// Start writer goroutine.
	go func() {
		defer close(writerDone)
		consecutivePriority := 0
		send := func(msg *protocol.Message) bool {
			if err := t.writeMessageOnConn(conn, msg); err != nil {
				log.Printf("[tunnel] write error: %v", err)
				return false
			}
			if protocol.NormalizePriority(msg.Priority) == protocol.PriorityPriority {
				consecutivePriority++
			} else {
				consecutivePriority = 0
			}
			return true
		}
		for {
			if msg, ok := t.tryDequeueNextMessage(consecutivePriority); ok {
				if !send(msg) {
					return
				}
				continue
			}

			select {
			case msg := <-t.priorityControlQ:
				if t.shouldDropQueuedControlMessage(msg) {
					continue
				}
				if !send(msg) {
					return
				}
			case <-t.outputReady:
				continue
			case msg := <-t.normalControlQ:
				if t.shouldDropQueuedControlMessage(msg) {
					continue
				}
				if !send(msg) {
					return
				}
			case <-t.ctx.Done():
				return
			case <-heartbeatStop:
				return
			}
		}
	}()

	// Reader loop
	for {
		select {
		case <-t.ctx.Done():
			return nil
		default:
		}

		_, data, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		var msg protocol.Message
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("[tunnel] invalid message: %v", err)
			continue
		}
		msg.Priority = protocol.NormalizePriority(msg.Priority)

		if msg.Type == protocol.TypePong {
			continue
		}

		// Handle async to not block the reader.
		go func(m protocol.Message, generation uint64) {
			m.Generation = generation
			if generation != 0 && generation != t.currentRuntimeGeneration() {
				return
			}
			resp := t.handler.Handle(&m)
			if resp != nil {
				t.enqueueControlForGeneration(generation, resp)
			}
		}(msg, generation)
	}
}

func (t *Tunnel) enqueueControlForGeneration(generation uint64, msg *protocol.Message) {
	if msg == nil {
		return
	}
	runtime, err := t.runtimeSnapshotForGeneration(generation)
	if err != nil {
		return
	}
	msg.Priority = protocol.NormalizePriority(msg.Priority)
	msg.Generation = generation
	target := t.normalControlQ
	if msg.Priority == protocol.PriorityPriority {
		target = t.priorityControlQ
	}
	for {
		select {
		case target <- msg:
			return
		case <-runtime.ctx.Done():
			if generation != 0 && generation != t.currentRuntimeGeneration() {
				return
			}
			// Generation-zero callers are intentionally unbound; let them retry using
			// the same queue behavior as a normal enqueue.
		case <-t.ctx.Done():
			return
		}
	}
}

func (t *Tunnel) shouldDropQueuedControlMessage(msg *protocol.Message) bool {
	if msg == nil {
		return true
	}
	// Control queues outlive a single websocket generation, so a blocked
	// old-generation sender can still win the reset race and enqueue after we
	// swapped runtimes. Drop those stale entries before a writer can forward them
	// onto the next connection.
	if msg.Generation != 0 && msg.Generation != t.currentRuntimeGeneration() {
		return true
	}
	return false
}

func (t *Tunnel) enqueueControl(msg *protocol.Message) {
	if msg == nil {
		return
	}
	msg.Priority = protocol.NormalizePriority(msg.Priority)
	target := t.normalControlQ
	if msg.Priority == protocol.PriorityPriority {
		target = t.priorityControlQ
	}
	select {
	case target <- msg:
	case <-t.ctx.Done():
	}
}

func (t *Tunnel) enqueueOutput(sessionID, priority string, msg *protocol.Message, size int) {
	t.enqueueOutputForGeneration(t.currentRuntimeGeneration(), sessionID, priority, msg, size)
}

func (t *Tunnel) enqueueOutputForSession(namespace outputSessionNamespace, sessionID, priority string, msg *protocol.Message, size int) {
	t.enqueueOutputForSessionForGeneration(t.currentRuntimeGeneration(), namespace, sessionID, priority, msg, size)
}

func (t *Tunnel) enqueueOutputForSessionForGeneration(generation uint64, namespace outputSessionNamespace, sessionID, priority string, msg *protocol.Message, size int) {
	t.enqueueOutputForGeneration(generation, outputSessionQueueKey(namespace, sessionID), priority, msg, size)
}

func (t *Tunnel) enqueueOutputForGeneration(generation uint64, sessionID, priority string, msg *protocol.Message, size int) {
	if msg == nil {
		return
	}
	if generation != 0 && generation != t.currentRuntimeGeneration() {
		return
	}
	priority = protocol.NormalizePriority(priority)
	msg.Priority = priority
	if size <= 0 {
		size = len(msg.Payload)
	}

	t.outputMu.Lock()
	q := t.outputQueues[sessionID]
	if q == nil {
		q = &sessionOutputQueue{priority: priority}
		q.full = sync.NewCond(&t.outputMu)
		t.outputQueues[sessionID] = q
	} else {
		q.priority = priority
	}
	if _, completed := t.completedOutputSessions[sessionID]; completed {
		q.drainWhenEmpty = true
	}
	q.writers++
	finishWrite := func() {
		q.writers--
		t.maybeDeleteOutputQueueLocked(sessionID, q)
		t.outputMu.Unlock()
	}

	for {
		if q.closed || (generation != 0 && generation != t.currentRuntimeGeneration()) {
			finishWrite()
			return
		}
		if !t.sessionQueueWouldBlock(q, size) {
			break
		}
		if t.ctx.Err() != nil {
			finishWrite()
			return
		}
		q.full.Wait()
	}
	if q.closed || (generation != 0 && generation != t.currentRuntimeGeneration()) {
		finishWrite()
		return
	}

	q.items = append(q.items, queuedOutput{msg: msg, size: size})
	q.bytes += size
	if !q.active {
		q.active = true
		if q.priority == protocol.PriorityPriority {
			t.activePriorityOutputs = append(t.activePriorityOutputs, sessionID)
		} else {
			t.activeNormalOutputs = append(t.activeNormalOutputs, sessionID)
		}
	}
	finishWrite()

	select {
	case t.outputReady <- struct{}{}:
	default:
	}
}

func (t *Tunnel) sessionQueueWouldBlock(q *sessionOutputQueue, incomingSize int) bool {
	if len(q.items) >= sessionQueueMaxMessages {
		return true
	}
	if incomingSize > sessionQueueMaxBytes {
		return len(q.items) > 0
	}
	return q.bytes+incomingSize > sessionQueueMaxBytes
}

func (t *Tunnel) tryDequeueControl(priority string) (*protocol.Message, bool) {
	source := t.normalControlQ
	if protocol.NormalizePriority(priority) == protocol.PriorityPriority {
		source = t.priorityControlQ
	}
	for {
		select {
		case msg := <-source:
			if t.shouldDropQueuedControlMessage(msg) {
				continue
			}
			return msg, true
		default:
			return nil, false
		}
	}
}

func (t *Tunnel) tryDequeueNormalMessage() (*protocol.Message, bool) {
	if msg, ok := t.tryDequeueControl(protocol.PriorityNormal); ok {
		return msg, true
	}
	return t.dequeueOutputForPriority(protocol.PriorityNormal)
}

func (t *Tunnel) tryDequeueNextMessage(consecutivePriority int) (*protocol.Message, bool) {
	if consecutivePriority >= priorityBurstLimit {
		if msg, ok := t.tryDequeueNormalMessage(); ok {
			return msg, true
		}
	}
	if msg, ok := t.tryDequeueControl(protocol.PriorityPriority); ok {
		return msg, true
	}
	if msg, ok := t.dequeueOutputForPriority(protocol.PriorityPriority); ok {
		return msg, true
	}
	return t.tryDequeueNormalMessage()
}

func (t *Tunnel) dequeueOutputForPriority(priority string) (*protocol.Message, bool) {
	t.outputMu.Lock()
	defer t.outputMu.Unlock()
	return t.dequeueOutputForPriorityLocked(protocol.NormalizePriority(priority))
}

func (t *Tunnel) dequeueOutputForPriorityLocked(priority string) (*protocol.Message, bool) {
	activeOutputs := &t.activeNormalOutputs
	rrIndex := &t.normalRRIndex
	if priority == protocol.PriorityPriority {
		activeOutputs = &t.activePriorityOutputs
		rrIndex = &t.priorityRRIndex
	}

	for len(*activeOutputs) > 0 {
		idx := *rrIndex % len(*activeOutputs)
		sessionID := (*activeOutputs)[idx]
		q := t.outputQueues[sessionID]
		if q == nil || len(q.items) == 0 {
			t.removeActiveOutputAtLocked(activeOutputs, rrIndex, idx)
			continue
		}

		item := q.items[0]
		q.items = q.items[1:]
		q.bytes -= item.size
		if q.bytes < 0 {
			q.bytes = 0
		}
		q.full.Signal()

		if len(q.items) == 0 {
			t.removeActiveOutputAtLocked(activeOutputs, rrIndex, idx)
		} else {
			*rrIndex = (idx + 1) % len(*activeOutputs)
		}
		return item.msg, true
	}

	*rrIndex = 0
	return nil, false
}

func (t *Tunnel) removeActiveOutputAtLocked(activeOutputs *[]string, rrIndex *int, idx int) {
	if idx < 0 || idx >= len(*activeOutputs) {
		return
	}
	sessionID := (*activeOutputs)[idx]
	q := t.outputQueues[sessionID]
	if q != nil {
		q.active = false
		t.maybeDeleteOutputQueueLocked(sessionID, q)
	}

	*activeOutputs = append((*activeOutputs)[:idx], (*activeOutputs)[idx+1:]...)
	if len(*activeOutputs) == 0 {
		*rrIndex = 0
		return
	}
	if idx < *rrIndex {
		*rrIndex--
	}
	if *rrIndex >= len(*activeOutputs) {
		*rrIndex = 0
	}
}

// writeMessage sends a message over the WebSocket.
func (t *Tunnel) writeMessage(msg *protocol.Message) error {
	t.connMu.Lock()
	conn := t.conn
	t.connMu.Unlock()
	return t.writeMessageOnConn(conn, msg)
}

func (t *Tunnel) writeMessageOnConn(conn *websocket.Conn, msg *protocol.Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	t.connMu.Lock()
	defer t.connMu.Unlock()
	if conn == nil || t.conn == nil {
		return fmt.Errorf("not connected")
	}
	if conn != t.conn {
		return fmt.Errorf("stale connection")
	}
	return conn.WriteMessage(websocket.TextMessage, data)
}

func (t *Tunnel) wsURL() string {
	server := t.cfg.Server
	server = strings.TrimSuffix(server, "/")
	if strings.HasPrefix(server, "https://") {
		return "wss://" + server[8:] + "/api/relay/ws"
	}
	if strings.HasPrefix(server, "http://") {
		return "ws://" + server[7:] + "/api/relay/ws"
	}
	return "wss://" + server + "/api/relay/ws"
}

// --- Shell message handlers ---

func (t *Tunnel) handleShellCreate(msg *protocol.Message) (string, any, error) {
	shells, err := t.shellsForMessage(msg)
	if err != nil {
		return protocol.TypeShellCreate + ".result", nil, err
	}
	var p protocol.ShellCreatePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeShellCreate + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}

	if t.outputSessionStateExists(outputSessionNamespaceShell, p.SessionID) {
		return protocol.TypeShellCreate + ".result", nil, fmt.Errorf("shell session %s still has queued output from the previous run", p.SessionID)
	}
	_, err = shells.Create(p.SessionID, p.Shell, p.Args, p.CWD, p.Env)
	if err != nil {
		return protocol.TypeShellCreate + ".result", nil, err
	}

	return protocol.TypeShellCreate + ".result", map[string]string{"session_id": p.SessionID, "status": "created"}, nil
}

func (t *Tunnel) handleShellExec(msg *protocol.Message) (string, any, error) {
	shells, err := t.shellsForMessage(msg)
	if err != nil {
		return protocol.TypeShellExecResult, nil, err
	}
	var p protocol.ShellExecPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeShellExecResult, nil, fmt.Errorf("parse payload: %w", err)
	}

	sess, err := shells.Get(p.SessionID)
	if err != nil {
		return protocol.TypeShellExecResult, nil, err
	}

	stdout, exitCode, duration, err := sess.Exec(p.Command, p.Timeout)
	if err != nil {
		return protocol.TypeShellExecResult, nil, err
	}

	return protocol.TypeShellExecResult, &protocol.ShellExecResultPayload{
		SessionID:  p.SessionID,
		Stdout:     stdout,
		ExitCode:   exitCode,
		DurationMs: duration.Milliseconds(),
	}, nil
}

func (t *Tunnel) handleShellKill(msg *protocol.Message) (string, any, error) {
	shells, err := t.shellsForMessage(msg)
	if err != nil {
		return protocol.TypeShellKill + ".result", nil, err
	}
	var p protocol.ShellKillPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeShellKill + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}

	if err := shells.Kill(p.SessionID); err != nil {
		return protocol.TypeShellKill + ".result", nil, err
	}

	return protocol.TypeShellKill + ".result", map[string]string{"status": "killed"}, nil
}

func (t *Tunnel) handleShellWriteStdin(msg *protocol.Message) (string, any, error) {
	shells, err := t.shellsForMessage(msg)
	if err != nil {
		return protocol.TypeShellWriteStdin + ".result", nil, err
	}
	var p protocol.ShellWriteStdinPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeShellWriteStdin + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}

	sess, err := shells.Get(p.SessionID)
	if err != nil {
		return protocol.TypeShellWriteStdin + ".result", nil, err
	}

	if err := sess.WriteStdinBase64(p.Data); err != nil {
		return protocol.TypeShellWriteStdin + ".result", nil, err
	}

	return protocol.TypeShellWriteStdin + ".result", map[string]string{"status": "ok"}, nil
}

func (t *Tunnel) handleShellResize(msg *protocol.Message) (string, any, error) {
	shells, err := t.shellsForMessage(msg)
	if err != nil {
		return protocol.TypeShellResize + ".result", nil, err
	}
	var p protocol.ShellResizePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeShellResize + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}

	sess, err := shells.Get(p.SessionID)
	if err != nil {
		return protocol.TypeShellResize + ".result", nil, err
	}

	if err := sess.Resize(p.Cols, p.Rows); err != nil {
		return protocol.TypeShellResize + ".result", nil, err
	}

	return protocol.TypeShellResize + ".result", map[string]string{"status": "ok"}, nil
}

func (t *Tunnel) handleShellSignal(msg *protocol.Message) (string, any, error) {
	shells, err := t.shellsForMessage(msg)
	if err != nil {
		return protocol.TypeShellSignal + ".result", nil, err
	}
	var p protocol.ShellSignalPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeShellSignal + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}

	sess, err := shells.Get(p.SessionID)
	if err != nil {
		return protocol.TypeShellSignal + ".result", nil, err
	}

	if err := sess.Signal(p.Signal); err != nil {
		return protocol.TypeShellSignal + ".result", nil, err
	}

	return protocol.TypeShellSignal + ".result", map[string]string{"status": "ok"}, nil
}

// --- Proc message handlers ---

func (t *Tunnel) handleProcRun(msg *protocol.Message) (string, any, error) {
	runtime, err := t.runtimeSnapshotForMessage(msg)
	if err != nil {
		return protocol.TypeProcRunResult, nil, err
	}
	if runtime.procs == nil {
		return protocol.TypeProcRunResult, nil, fmt.Errorf("proc multiplexer not initialized")
	}
	var p protocol.ProcRunPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeProcRunResult, nil, fmt.Errorf("parse payload: %w", err)
	}
	var stdin []byte
	if p.Stdin != "" {
		var err error
		stdin, err = base64.StdEncoding.DecodeString(p.Stdin)
		if err != nil {
			return protocol.TypeProcRunResult, nil, fmt.Errorf("decode stdin: %w", err)
		}
	}
	runCtx := runtime.ctx
	requestGeneration := t.requestGeneration(msg)
	cancelKey := ""
	if msg != nil {
		cancelKey = procRunCancelID(p.RunID, msg.ID)
	}
	if cancelKey != "" {
		var cancel context.CancelCauseFunc
		runCtx, cancel = context.WithCancelCause(runtime.ctx)
		if err := t.rememberProcRunCancel(cancelKey, requestGeneration, cancel); err != nil {
			cancel(nil)
			return protocol.TypeProcRunResult, nil, err
		}
		defer func() {
			t.forgetProcRunCancel(cancelKey, requestGeneration)
			cancel(nil)
		}()
	}
	result, err := proc.Run(runCtx, p.Argv, p.CWD, p.Env, stdin, p.TimeoutMs)
	payload := procRunResultPayload(result, false)
	if err != nil {
		if payload != nil && errors.Is(err, context.Canceled) && errors.Is(context.Cause(runCtx), procRunCanceledCause) {
			payload.Canceled = true
			return protocol.TypeProcRunResult, payload, nil
		}
		return protocol.TypeProcRunResult, nil, err
	}
	return protocol.TypeProcRunResult, payload, nil
}

func (t *Tunnel) handleProcCancel(msg *protocol.Message) (string, any, error) {
	var p protocol.ProcCancelPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeProcCancelResult, nil, fmt.Errorf("parse payload: %w", err)
	}
	generation := t.requestGeneration(msg)
	status := "not_found"
	matchedField := ""
	matchedID := ""
	if p.RunID != "" && t.cancelProcRun(p.RunID, generation) {
		status = "canceled"
		matchedField = "run_id"
		matchedID = p.RunID
	} else if p.RequestID != "" && t.cancelProcRun(p.RequestID, generation) {
		status = "canceled"
		matchedField = "request_id"
		matchedID = p.RequestID
	}
	result := map[string]string{"status": status}
	if matchedField != "" {
		result[matchedField] = matchedID
	}
	return protocol.TypeProcCancelResult, result, nil
}

func (t *Tunnel) handleProcSpawn(msg *protocol.Message) (string, any, error) {
	procs, err := t.procsForMessage(msg)
	if err != nil {
		return protocol.TypeProcSpawnResult, nil, err
	}
	var p protocol.ProcSpawnPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeProcSpawnResult, nil, fmt.Errorf("parse payload: %w", err)
	}
	if t.outputSessionStateExists(outputSessionNamespaceProc, p.SessionID) {
		return protocol.TypeProcSpawnResult, nil, fmt.Errorf("proc session %s still has queued output from the previous run", p.SessionID)
	}
	priority := protocol.NormalizePriority(msg.Priority)
	log.Printf("[relay-node] rpc proc.spawn session=%s argv=%v pty=%t priority=%s", p.SessionID, p.Argv, p.Pty, priority)
	if err := procs.Spawn(p.SessionID, p.Argv, p.CWD, p.Env, p.Pty, p.Cols, p.Rows, priority); err != nil {
		return protocol.TypeProcSpawnResult, nil, err
	}
	return protocol.TypeProcSpawnResult, map[string]string{"status": "started", "session_id": p.SessionID}, nil
}

func (t *Tunnel) handleProcStdin(msg *protocol.Message) (string, any, error) {
	procs, err := t.procsForMessage(msg)
	if err != nil {
		return protocol.TypeProcStdinResult, nil, err
	}
	var p protocol.ProcStdinPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeProcStdinResult, nil, fmt.Errorf("parse payload: %w", err)
	}
	log.Printf("[relay-node] rpc proc.stdin session=%s eof=%t", p.SessionID, p.EOF)
	if p.Data != "" {
		sess, err := procs.Get(p.SessionID)
		if err != nil {
			return protocol.TypeProcStdinResult, nil, err
		}
		if err := sess.WriteStdinBase64(p.Data); err != nil {
			return protocol.TypeProcStdinResult, nil, err
		}
	}
	if p.EOF {
		if err := procs.CloseStdin(p.SessionID); err != nil {
			return protocol.TypeProcStdinResult, nil, err
		}
	}
	return protocol.TypeProcStdinResult, map[string]string{"status": "ok"}, nil
}

func (t *Tunnel) handleProcResize(msg *protocol.Message) (string, any, error) {
	procs, err := t.procsForMessage(msg)
	if err != nil {
		return protocol.TypeProcResizeResult, nil, err
	}
	var p protocol.ProcResizePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeProcResizeResult, nil, fmt.Errorf("parse payload: %w", err)
	}
	log.Printf("[relay-node] rpc proc.resize session=%s cols=%d rows=%d", p.SessionID, p.Cols, p.Rows)
	if err := procs.Resize(p.SessionID, p.Cols, p.Rows); err != nil {
		return protocol.TypeProcResizeResult, nil, err
	}
	return protocol.TypeProcResizeResult, map[string]string{"status": "ok"}, nil
}

func (t *Tunnel) handleProcSignal(msg *protocol.Message) (string, any, error) {
	procs, err := t.procsForMessage(msg)
	if err != nil {
		return protocol.TypeProcSignalResult, nil, err
	}
	var p protocol.ProcSignalPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeProcSignalResult, nil, fmt.Errorf("parse payload: %w", err)
	}
	log.Printf("[relay-node] rpc proc.signal session=%s signal=%s", p.SessionID, p.Signal)
	if err := procs.Signal(p.SessionID, p.Signal); err != nil {
		return protocol.TypeProcSignalResult, nil, err
	}
	return protocol.TypeProcSignalResult, map[string]string{"status": "ok"}, nil
}

func (t *Tunnel) handleProcKill(msg *protocol.Message) (string, any, error) {
	procs, err := t.procsForMessage(msg)
	if err != nil {
		return protocol.TypeProcKillResult, nil, err
	}
	var p protocol.ProcKillPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeProcKillResult, nil, fmt.Errorf("parse payload: %w", err)
	}
	log.Printf("[relay-node] rpc proc.kill session=%s", p.SessionID)
	if err := procs.Kill(p.SessionID); err != nil {
		return protocol.TypeProcKillResult, nil, err
	}
	return protocol.TypeProcKillResult, map[string]string{"status": "ok"}, nil
}

func (t *Tunnel) handleProcCheckAlive(msg *protocol.Message) (string, any, error) {
	runtime, err := t.runtimeSnapshotForMessage(msg)
	if err != nil {
		return protocol.TypeProcCheckAliveResult, nil, err
	}
	var p protocol.ProcKillPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeProcCheckAliveResult, nil, fmt.Errorf("parse payload: %w", err)
	}
	alive := false
	if runtime.procs != nil {
		alive = runtime.procs.CheckAlive(p.SessionID)
	}
	log.Printf("[relay-node] rpc proc.check_alive session=%s alive=%t", p.SessionID, alive)
	return protocol.TypeProcCheckAliveResult, map[string]bool{"alive": alive}, nil
}

// --- Container message handlers ---

func (t *Tunnel) handleContainerEnsure(msg *protocol.Message) (string, any, error) {
	runtime, err := t.runtimeSnapshotForMessage(msg)
	if err != nil {
		return protocol.TypeContainerEnsure + ".result", nil, err
	}
	if runtime.execs == nil {
		return protocol.TypeContainerEnsure + ".result", nil, fmt.Errorf("exec multiplexer not initialized")
	}
	if t.docker == nil {
		return protocol.TypeContainerEnsure + ".result", nil, fmt.Errorf("docker client not initialized")
	}
	var p protocol.ContainerEnsurePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeContainerEnsure + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}
	image := p.Image
	if image == "" {
		image = p.DockerImage
	}
	log.Printf("[relay-node] rpc container.ensure container=%s image=%s user=%s agent=%s", p.ContainerID, image, p.UserID, p.AgentID)
	err = runtime.execs.WithContainerMutation(p.ContainerID, func() error {
		status, err := t.docker.ContainerStatusContext(runtime.ctx, p.ContainerID)
		if err != nil {
			return err
		}
		if status == docker.StatusRunning {
			return nil
		}
		if err := t.terminateContainerExecSessions(runtime.execs, p.ContainerID, protocol.TypeContainerEnsure); err != nil {
			return fmt.Errorf("terminate container exec sessions: %w", err)
		}
		return t.docker.EnsureContainerContext(runtime.ctx, docker.EnsureOptions{
			ContainerID: p.ContainerID,
			Image:       image,
			UserID:      p.UserID,
			AgentID:     p.AgentID,
			RootAgentID: p.RootAgentID,
			ResourceLimits: docker.ResourceLimits{
				CPUs:      p.ResourceLimits.CPUs,
				MemoryMB:  p.ResourceLimits.MemoryMB,
				PidsLimit: p.ResourceLimits.PidsLimit,
			},
			Mounts: mapMounts(p.Mounts),
		})
	})
	if err != nil {
		return protocol.TypeContainerEnsure + ".result", nil, err
	}
	return protocol.TypeContainerEnsure + ".result", map[string]string{"status": "running", "container_id": p.ContainerID}, nil
}

func mapMounts(in []protocol.DockerMount) []docker.Mount {
	out := make([]docker.Mount, 0, len(in))
	for _, m := range in {
		out = append(out, docker.Mount{Host: m.Host, Container: m.Container, Readonly: m.Readonly})
	}
	return out
}

func (t *Tunnel) handleContainerExec(msg *protocol.Message) (string, any, error) {
	execs, err := t.execsForMessage(msg)
	if err != nil {
		return protocol.TypeContainerExec + ".result", nil, err
	}
	var p protocol.ContainerExecPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeContainerExec + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}
	if t.outputSessionStateExists(outputSessionNamespaceContainerExec, p.SessionID) {
		return protocol.TypeContainerExec + ".result", nil, fmt.Errorf("container exec session %s still has queued output from the previous run", p.SessionID)
	}
	log.Printf("[relay-node] rpc container.exec container=%s session=%s command=%s", p.ContainerID, p.SessionID, p.Command)
	if err := execs.Create(p.ContainerID, p.SessionID, p.Command, p.Args, p.CWD, p.Env, p.Tty, p.Cols, p.Rows); err != nil {
		return protocol.TypeContainerExec + ".result", nil, err
	}
	return protocol.TypeContainerExec + ".result", map[string]string{"status": "started", "session_id": p.SessionID}, nil
}

func (t *Tunnel) handleContainerExecStdin(msg *protocol.Message) (string, any, error) {
	execs, err := t.execsForMessage(msg)
	if err != nil {
		return protocol.TypeContainerExecStdin + ".result", nil, err
	}
	var p protocol.ContainerExecStdinPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeContainerExecStdin + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}
	log.Printf("[relay-node] rpc container.exec.stdin session=%s", p.SessionID)
	sess, err := execs.Get(p.SessionID)
	if err != nil {
		return protocol.TypeContainerExecStdin + ".result", nil, err
	}
	if err := sess.WriteStdinBase64(p.Data); err != nil {
		return protocol.TypeContainerExecStdin + ".result", nil, err
	}
	return protocol.TypeContainerExecStdin + ".result", nil, nil
}

func (t *Tunnel) handleContainerExecResize(msg *protocol.Message) (string, any, error) {
	execs, err := t.execsForMessage(msg)
	if err != nil {
		return protocol.TypeContainerExecResize + ".result", nil, err
	}
	var p protocol.ContainerExecResizePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeContainerExecResize + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}
	log.Printf("[relay-node] rpc container.exec.resize session=%s cols=%d rows=%d", p.SessionID, p.Cols, p.Rows)
	if err := execs.Resize(p.SessionID, p.Cols, p.Rows); err != nil {
		return protocol.TypeContainerExecResize + ".result", nil, err
	}
	return protocol.TypeContainerExecResize + ".result", nil, nil
}

func (t *Tunnel) handleContainerExecSignal(msg *protocol.Message) (string, any, error) {
	execs, err := t.execsForMessage(msg)
	if err != nil {
		return protocol.TypeContainerExecSignal + ".result", nil, err
	}
	var p protocol.ContainerExecSignalPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeContainerExecSignal + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}
	log.Printf("[relay-node] rpc container.exec.signal session=%s signal=%s shell_pid=%d", p.SessionID, p.Signal, p.ShellPID)
	sess, err := execs.Get(p.SessionID)
	if err != nil {
		return protocol.TypeContainerExecSignal + ".result", nil, err
	}
	if err := sess.Signal(p.Signal, p.ShellPID); err != nil {
		return protocol.TypeContainerExecSignal + ".result", nil, err
	}
	return protocol.TypeContainerExecSignal + ".result", nil, nil
}

func (t *Tunnel) handleContainerExecKill(msg *protocol.Message) (string, any, error) {
	execs, err := t.execsForMessage(msg)
	if err != nil {
		return protocol.TypeContainerExecKill + ".result", nil, err
	}
	var p protocol.ContainerExecKillPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeContainerExecKill + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}
	log.Printf("[relay-node] rpc container.exec.kill session=%s", p.SessionID)
	if err := execs.Kill(p.SessionID); err != nil {
		return protocol.TypeContainerExecKill + ".result", nil, err
	}
	return protocol.TypeContainerExecKill + ".result", nil, nil
}

func (t *Tunnel) handleContainerExecCheckAlive(msg *protocol.Message) (string, any, error) {
	runtime, err := t.runtimeSnapshotForMessage(msg)
	if err != nil {
		return protocol.TypeContainerExecCheckAlive + ".result", nil, err
	}
	var p protocol.ContainerExecKillPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeContainerExecCheckAlive + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}
	alive := runtime.execs != nil && runtime.execs.CheckAlive(p.SessionID)
	log.Printf("[relay-node] rpc container.exec.check_alive session=%s alive=%t", p.SessionID, alive)
	return protocol.TypeContainerExecCheckAlive + ".result", map[string]bool{"alive": alive}, nil
}

func (t *Tunnel) terminateContainerExecSessions(execs *docker.ExecMultiplexer, containerID, lifecycleOp string) error {
	if execs == nil {
		return nil
	}
	killed, err := execs.KillByContainer(containerID, containerExecReapTimeoutMs)
	if err != nil {
		log.Printf("[relay-node] rpc %s container=%s exec_cleanup_failed killed=%d err=%v", lifecycleOp, containerID, killed, err)
		return err
	}
	if killed > 0 {
		log.Printf("[relay-node] rpc %s container=%s exec_cleanup_killed=%d", lifecycleOp, containerID, killed)
	}
	return nil
}

func (t *Tunnel) withContainerMutationUsingExecs(execs *docker.ExecMultiplexer, containerID, lifecycleOp string, action func() error) error {
	if execs == nil {
		return action()
	}
	return execs.WithContainerMutation(containerID, func() error {
		if err := t.terminateContainerExecSessions(execs, containerID, lifecycleOp); err != nil {
			return fmt.Errorf("terminate container exec sessions: %w", err)
		}
		return action()
	})
}

func (t *Tunnel) withContainerMutation(containerID, lifecycleOp string, action func() error) error {
	return t.withContainerMutationUsingExecs(t.execs, containerID, lifecycleOp, action)
}

func (t *Tunnel) handleContainerPause(msg *protocol.Message) (string, any, error) {
	runtime, err := t.runtimeSnapshotForMessage(msg)
	if err != nil {
		return protocol.TypeContainerPause + ".result", nil, err
	}
	if runtime.execs == nil {
		return protocol.TypeContainerPause + ".result", nil, fmt.Errorf("exec multiplexer not initialized")
	}
	var p protocol.ContainerIDPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeContainerPause + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}
	log.Printf("[relay-node] rpc container.pause container=%s", p.ContainerID)
	if err := t.withContainerMutationUsingExecs(runtime.execs, p.ContainerID, "container.pause", func() error {
		return t.docker.PauseContext(runtime.ctx, p.ContainerID)
	}); err != nil {
		return protocol.TypeContainerPause + ".result", nil, err
	}
	return protocol.TypeContainerPause + ".result", map[string]string{"status": "ok"}, nil
}

func (t *Tunnel) handleContainerUnpause(msg *protocol.Message) (string, any, error) {
	runtime, err := t.runtimeSnapshotForMessage(msg)
	if err != nil {
		return protocol.TypeContainerUnpause + ".result", nil, err
	}
	if runtime.execs == nil {
		return protocol.TypeContainerUnpause + ".result", nil, fmt.Errorf("exec multiplexer not initialized")
	}
	var p protocol.ContainerIDPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeContainerUnpause + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}
	log.Printf("[relay-node] rpc container.unpause container=%s", p.ContainerID)
	if err := t.withContainerMutationUsingExecs(runtime.execs, p.ContainerID, "container.unpause", func() error {
		return t.docker.UnpauseContext(runtime.ctx, p.ContainerID)
	}); err != nil {
		return protocol.TypeContainerUnpause + ".result", nil, err
	}
	return protocol.TypeContainerUnpause + ".result", map[string]string{"status": "ok"}, nil
}

func (t *Tunnel) handleContainerStop(msg *protocol.Message) (string, any, error) {
	runtime, err := t.runtimeSnapshotForMessage(msg)
	if err != nil {
		return protocol.TypeContainerStop + ".result", nil, err
	}
	if runtime.execs == nil {
		return protocol.TypeContainerStop + ".result", nil, fmt.Errorf("exec multiplexer not initialized")
	}
	var p protocol.ContainerIDPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeContainerStop + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}
	log.Printf("[relay-node] rpc container.stop container=%s", p.ContainerID)
	if err := t.withContainerMutationUsingExecs(runtime.execs, p.ContainerID, "container.stop", func() error {
		return t.docker.StopContext(runtime.ctx, p.ContainerID)
	}); err != nil {
		return protocol.TypeContainerStop + ".result", nil, err
	}
	return protocol.TypeContainerStop + ".result", map[string]string{"status": "ok"}, nil
}

func (t *Tunnel) handleContainerDestroy(msg *protocol.Message) (string, any, error) {
	runtime, err := t.runtimeSnapshotForMessage(msg)
	if err != nil {
		return protocol.TypeContainerDestroy + ".result", nil, err
	}
	if runtime.execs == nil {
		return protocol.TypeContainerDestroy + ".result", nil, fmt.Errorf("exec multiplexer not initialized")
	}
	var p protocol.ContainerIDPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeContainerDestroy + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}
	log.Printf("[relay-node] rpc container.destroy container=%s", p.ContainerID)
	if err := t.withContainerMutationUsingExecs(runtime.execs, p.ContainerID, "container.destroy", func() error {
		return t.docker.DestroyContext(runtime.ctx, p.ContainerID)
	}); err != nil {
		return protocol.TypeContainerDestroy + ".result", nil, err
	}
	return protocol.TypeContainerDestroy + ".result", map[string]string{"status": "ok"}, nil
}

func (t *Tunnel) handleContainerStatus(msg *protocol.Message) (string, any, error) {
	runtime, err := t.runtimeSnapshotForMessage(msg)
	if err != nil {
		return protocol.TypeContainerStatus + ".result", nil, err
	}
	var p protocol.ContainerIDPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeContainerStatus + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}
	status, err := t.docker.ContainerStatusContext(runtime.ctx, p.ContainerID)
	if err != nil {
		return protocol.TypeContainerStatus + ".result", nil, err
	}
	log.Printf("[relay-node] rpc container.status container=%s status=%s", p.ContainerID, status)
	return protocol.TypeContainerStatus + ".result", map[string]string{"status": status}, nil
}

func (t *Tunnel) handleContainerExecCommand(msg *protocol.Message) (string, any, error) {
	runtime, err := t.runtimeSnapshotForMessage(msg)
	if err != nil {
		return protocol.TypeContainerExecCommand + ".result", nil, err
	}
	if runtime.execs == nil {
		return protocol.TypeContainerExecCommand + ".result", nil, fmt.Errorf("exec multiplexer not initialized")
	}
	if t.docker == nil {
		return protocol.TypeContainerExecCommand + ".result", nil, fmt.Errorf("docker client not initialized")
	}
	var p protocol.ContainerExecCommandPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeContainerExecCommand + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}
	log.Printf("[relay-node] rpc container.exec_command container=%s command=%v", p.ContainerID, p.Command)
	output := ""
	err = runtime.execs.WithContainerExec(p.ContainerID, func() error {
		var execErr error
		output, execErr = t.docker.ExecCommandContext(runtime.ctx, p.ContainerID, p.Command)
		return execErr
	})
	if err != nil {
		return protocol.TypeContainerExecCommand + ".result", nil, err
	}
	return protocol.TypeContainerExecCommand + ".result", map[string]string{"output": output}, nil
}

func (t *Tunnel) handleContainerExecWithStdin(msg *protocol.Message) (string, any, error) {
	runtime, err := t.runtimeSnapshotForMessage(msg)
	if err != nil {
		return protocol.TypeContainerExecWithStdin + ".result", nil, err
	}
	if runtime.execs == nil {
		return protocol.TypeContainerExecWithStdin + ".result", nil, fmt.Errorf("exec multiplexer not initialized")
	}
	if t.docker == nil {
		return protocol.TypeContainerExecWithStdin + ".result", nil, fmt.Errorf("docker client not initialized")
	}
	var p protocol.ContainerExecWithStdinPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeContainerExecWithStdin + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}
	log.Printf("[relay-node] rpc container.exec_with_stdin container=%s command=%v", p.ContainerID, p.Command)
	output := ""
	err = runtime.execs.WithContainerExec(p.ContainerID, func() error {
		var execErr error
		output, execErr = t.docker.ExecWithStdinContext(runtime.ctx, p.ContainerID, p.Command, p.Stdin)
		return execErr
	})
	if err != nil {
		return protocol.TypeContainerExecWithStdin + ".result", nil, err
	}
	return protocol.TypeContainerExecWithStdin + ".result", map[string]string{"output": output}, nil
}

// --- File message handlers ---

func (t *Tunnel) handleFileRead(msg *protocol.Message) (string, any, error) {
	runtime, err := t.runtimeSnapshotForMessage(msg)
	if err != nil {
		return protocol.TypeFileReadResult, nil, err
	}
	var p protocol.FileReadPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeFileReadResult, nil, err
	}
	result, err := fileops.ReadContext(runtime.ctx, &p)
	if err != nil {
		return protocol.TypeFileReadResult, nil, err
	}
	return protocol.TypeFileReadResult, result, nil
}

func (t *Tunnel) handleFileWrite(msg *protocol.Message) (string, any, error) {
	runtime, err := t.runtimeSnapshotForMessage(msg)
	if err != nil {
		return protocol.TypeFileWriteResult, nil, err
	}
	var p protocol.FileWritePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeFileWriteResult, nil, err
	}
	result, err := fileops.WriteContext(runtime.ctx, &p)
	if err != nil {
		return protocol.TypeFileWriteResult, nil, err
	}
	return protocol.TypeFileWriteResult, result, nil
}

func (t *Tunnel) handleFileStat(msg *protocol.Message) (string, any, error) {
	runtime, err := t.runtimeSnapshotForMessage(msg)
	if err != nil {
		return protocol.TypeFileStatResult, nil, err
	}
	var p protocol.FileStatPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeFileStatResult, nil, err
	}
	result, err := fileops.StatContext(runtime.ctx, &p)
	if err != nil {
		return protocol.TypeFileStatResult, nil, err
	}
	return protocol.TypeFileStatResult, result, nil
}

func (t *Tunnel) handleFileGlob(msg *protocol.Message) (string, any, error) {
	runtime, err := t.runtimeSnapshotForMessage(msg)
	if err != nil {
		return protocol.TypeFileGlobResult, nil, err
	}
	var p protocol.FileGlobPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeFileGlobResult, nil, err
	}
	result, err := fileops.GlobContext(runtime.ctx, &p)
	if err != nil {
		return protocol.TypeFileGlobResult, nil, err
	}
	return protocol.TypeFileGlobResult, result, nil
}

func (t *Tunnel) handleFileGrep(msg *protocol.Message) (string, any, error) {
	runtime, err := t.runtimeSnapshotForMessage(msg)
	if err != nil {
		return protocol.TypeFileGrepResult, nil, err
	}
	var p protocol.FileGrepPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeFileGrepResult, nil, err
	}
	result, err := fileops.GrepContext(runtime.ctx, &p)
	if err != nil {
		return protocol.TypeFileGrepResult, nil, err
	}
	return protocol.TypeFileGrepResult, result, nil
}

func (t *Tunnel) handleFileMkdir(msg *protocol.Message) (string, any, error) {
	runtime, err := t.runtimeSnapshotForMessage(msg)
	if err != nil {
		return protocol.TypeFileMkdirResult, nil, err
	}
	var p protocol.FileMkdirPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeFileMkdirResult, nil, err
	}
	if err := fileops.MkdirContext(runtime.ctx, &p); err != nil {
		return protocol.TypeFileMkdirResult, nil, err
	}
	return protocol.TypeFileMkdirResult, map[string]string{"status": "ok"}, nil
}

func (t *Tunnel) handleFileRemove(msg *protocol.Message) (string, any, error) {
	runtime, err := t.runtimeSnapshotForMessage(msg)
	if err != nil {
		return protocol.TypeFileRemoveResult, nil, err
	}
	var p protocol.FileRemovePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return protocol.TypeFileRemoveResult, nil, err
	}
	if err := fileops.RemoveContext(runtime.ctx, &p); err != nil {
		return protocol.TypeFileRemoveResult, nil, err
	}
	return protocol.TypeFileRemoveResult, map[string]string{"status": "ok"}, nil
}
