package tunnel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/taurusagents/taurus-relay/internal/auth"
	"github.com/taurusagents/taurus-relay/internal/config"
	"github.com/taurusagents/taurus-relay/internal/docker"
	"github.com/taurusagents/taurus-relay/internal/fileops"
	"github.com/taurusagents/taurus-relay/internal/health"
	"github.com/taurusagents/taurus-relay/internal/protocol"
	"github.com/taurusagents/taurus-relay/internal/shell"
)

type Mode string

const (
	ModeConnect Mode = "connect"
	ModeNode    Mode = "node"
)

type NodeOptions struct {
	Name          string
	Host          string
	Token         string
	DataPath      string
	MaxContainers int
}

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
	docker *docker.Client
	sendCh chan *protocol.Message
	stopCh chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
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

	t := &Tunnel{
		cfg:       cfg,
		token:     token,
		reconnCfg: DefaultReconnectConfig(),
		mode:      mode,
		node:      nodeOpts,
		handler:   protocol.NewHandler(),
		sendCh:    make(chan *protocol.Message, 256),
		stopCh:    make(chan struct{}),
		ctx:       ctx,
		cancel:    cancel,
	}

	if mode == ModeConnect {
		t.shells = shell.NewMultiplexer(
			func(sessionID string, data []byte) {
				payload, _ := json.Marshal(protocol.ShellOutputPayload{
					SessionID: sessionID,
					Data:      base64.StdEncoding.EncodeToString(data),
				})
				msg := &protocol.Message{Type: protocol.TypeShellOutput, Payload: payload}
				select {
				case t.sendCh <- msg:
				default:
					log.Printf("[tunnel] sendCh full, dropping shell.output for session %s", sessionID)
				}
			},
			func(sessionID string, exitCode int) {
				payload, _ := json.Marshal(protocol.ShellExitPayload{SessionID: sessionID, ExitCode: exitCode})
				t.sendCh <- &protocol.Message{Type: protocol.TypeShellExit, Payload: payload}
			},
		)
	}

	if mode == ModeNode && nodeOpts != nil {
		t.docker = docker.NewClient(nodeOpts.DataPath)
		t.execs = docker.NewExecMultiplexer(
			func(sessionID string, data []byte) {
				payload, _ := json.Marshal(protocol.ShellOutputPayload{
					SessionID: sessionID,
					Data:      base64.StdEncoding.EncodeToString(data),
				})
				msg := &protocol.Message{Type: protocol.TypeContainerExecOutput, Payload: payload}
				select {
				case t.sendCh <- msg:
				default:
					log.Printf("[tunnel] sendCh full, dropping container.exec.output for session %s", sessionID)
				}
			},
			func(sessionID string, exitCode int) {
				payload, _ := json.Marshal(protocol.ShellExitPayload{SessionID: sessionID, ExitCode: exitCode})
				t.sendCh <- &protocol.Message{Type: protocol.TypeContainerExecExit, Payload: payload}
			},
		)
	}

	t.registerHandlers(mode)
	return t
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
		h.Register(protocol.TypeContainerEnsure, t.handleContainerEnsure)
		h.Register(protocol.TypeContainerExec, t.handleContainerExec)
		h.Register(protocol.TypeContainerExecStdin, t.handleContainerExecStdin)
		h.Register(protocol.TypeContainerExecSignal, t.handleContainerExecSignal)
		h.Register(protocol.TypeContainerExecKill, t.handleContainerExecKill)
		h.Register(protocol.TypeContainerExecCheckAlive, t.handleContainerExecCheckAlive)
		h.Register(protocol.TypeContainerPause, t.handleContainerPause)
		h.Register(protocol.TypeContainerUnpause, t.handleContainerUnpause)
		h.Register(protocol.TypeContainerStop, t.handleContainerStop)
		h.Register(protocol.TypeContainerDestroy, t.handleContainerDestroy)
		h.Register(protocol.TypeContainerStatus, t.handleContainerStatus)
	}

	h.Register(protocol.TypeFileRead, t.handleFileRead)
	h.Register(protocol.TypeFileWrite, t.handleFileWrite)
	h.Register(protocol.TypeFileStat, t.handleFileStat)
	h.Register(protocol.TypeFileGlob, t.handleFileGlob)
	h.Register(protocol.TypeFileGrep, t.handleFileGrep)
	h.Register(protocol.TypeFileMkdir, t.handleFileMkdir)
	h.Register(protocol.TypeFileRemove, t.handleFileRemove)

	h.Register(protocol.TypePing, func(id string, _ json.RawMessage) (string, any, error) {
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
	if t.shells != nil {
		t.shells.KillAll()
	}
	if t.execs != nil {
		t.execs.KillAll()
	}
	t.connMu.Lock()
	if t.conn != nil {
		_ = t.conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "relay shutting down"))
		t.conn.Close()
	}
	t.connMu.Unlock()
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
		Meta: map[string]string{
			"os":       sys.OS,
			"arch":     sys.Arch,
			"hostname": hostname,
		},
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
			sessions = t.execs.Count()
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
	// Start heartbeat
	heartbeatStop := make(chan struct{})
	go health.HeartbeatLoop(30*time.Second, t.heartbeatInfo, t.sendCh, heartbeatStop)
	defer close(heartbeatStop)

	// Start writer goroutine
	go func() {
		for {
			select {
			case msg := <-t.sendCh:
				if err := t.writeMessage(msg); err != nil {
					log.Printf("[tunnel] write error: %v", err)
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

		_, data, err := t.conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		var msg protocol.Message
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("[tunnel] invalid message: %v", err)
			continue
		}

		// Handle async to not block the reader
		go func(m protocol.Message) {
			resp := t.handler.Handle(&m)
			if resp != nil {
				t.sendCh <- resp
			}
		}(msg)
	}
}

// writeMessage sends a message over the WebSocket.
func (t *Tunnel) writeMessage(msg *protocol.Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	t.connMu.Lock()
	defer t.connMu.Unlock()
	if t.conn == nil {
		return fmt.Errorf("not connected")
	}
	return t.conn.WriteMessage(websocket.TextMessage, data)
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

func (t *Tunnel) handleShellCreate(id string, payload json.RawMessage) (string, any, error) {
	var p protocol.ShellCreatePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return protocol.TypeShellCreate + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}

	_, err := t.shells.Create(p.SessionID, p.Shell, p.Args, p.CWD, p.Env)
	if err != nil {
		return protocol.TypeShellCreate + ".result", nil, err
	}

	return protocol.TypeShellCreate + ".result", map[string]string{"session_id": p.SessionID, "status": "created"}, nil
}

func (t *Tunnel) handleShellExec(id string, payload json.RawMessage) (string, any, error) {
	var p protocol.ShellExecPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return protocol.TypeShellExecResult, nil, fmt.Errorf("parse payload: %w", err)
	}

	sess, err := t.shells.Get(p.SessionID)
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

func (t *Tunnel) handleShellKill(id string, payload json.RawMessage) (string, any, error) {
	var p protocol.ShellKillPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return protocol.TypeShellKill + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}

	if err := t.shells.Kill(p.SessionID); err != nil {
		return protocol.TypeShellKill + ".result", nil, err
	}

	return protocol.TypeShellKill + ".result", map[string]string{"status": "killed"}, nil
}

func (t *Tunnel) handleShellWriteStdin(id string, payload json.RawMessage) (string, any, error) {
	var p protocol.ShellWriteStdinPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return protocol.TypeShellWriteStdin + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}

	sess, err := t.shells.Get(p.SessionID)
	if err != nil {
		return protocol.TypeShellWriteStdin + ".result", nil, err
	}

	if err := sess.WriteStdinBase64(p.Data); err != nil {
		return protocol.TypeShellWriteStdin + ".result", nil, err
	}

	return protocol.TypeShellWriteStdin + ".result", map[string]string{"status": "ok"}, nil
}

func (t *Tunnel) handleShellResize(id string, payload json.RawMessage) (string, any, error) {
	var p protocol.ShellResizePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return protocol.TypeShellResize + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}

	sess, err := t.shells.Get(p.SessionID)
	if err != nil {
		return protocol.TypeShellResize + ".result", nil, err
	}

	if err := sess.Resize(p.Cols, p.Rows); err != nil {
		return protocol.TypeShellResize + ".result", nil, err
	}

	return protocol.TypeShellResize + ".result", map[string]string{"status": "ok"}, nil
}

func (t *Tunnel) handleShellSignal(id string, payload json.RawMessage) (string, any, error) {
	var p protocol.ShellSignalPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return protocol.TypeShellSignal + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}

	sess, err := t.shells.Get(p.SessionID)
	if err != nil {
		return protocol.TypeShellSignal + ".result", nil, err
	}

	if err := sess.Signal(p.Signal); err != nil {
		return protocol.TypeShellSignal + ".result", nil, err
	}

	return protocol.TypeShellSignal + ".result", map[string]string{"status": "ok"}, nil
}

// --- Container message handlers ---

func (t *Tunnel) handleContainerEnsure(id string, payload json.RawMessage) (string, any, error) {
	if t.docker == nil {
		return protocol.TypeContainerEnsure + ".result", nil, fmt.Errorf("docker client not initialized")
	}
	var p protocol.ContainerEnsurePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return protocol.TypeContainerEnsure + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}
	image := p.Image
	if image == "" {
		image = p.DockerImage
	}
	err := t.docker.EnsureContainer(docker.EnsureOptions{
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

func (t *Tunnel) handleContainerExec(id string, payload json.RawMessage) (string, any, error) {
	if t.execs == nil {
		return protocol.TypeContainerExec + ".result", nil, fmt.Errorf("exec multiplexer not initialized")
	}
	var p protocol.ContainerExecPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return protocol.TypeContainerExec + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}
	if err := t.execs.Create(p.ContainerID, p.SessionID, p.Command, p.Args, p.CWD, p.Env); err != nil {
		return protocol.TypeContainerExec + ".result", nil, err
	}
	return protocol.TypeContainerExec + ".result", map[string]string{"status": "started", "session_id": p.SessionID}, nil
}

func (t *Tunnel) handleContainerExecStdin(id string, payload json.RawMessage) (string, any, error) {
	var p protocol.ContainerExecStdinPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return protocol.TypeContainerExecStdin + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}
	sess, err := t.execs.Get(p.SessionID)
	if err != nil {
		return protocol.TypeContainerExecStdin + ".result", nil, err
	}
	if err := sess.WriteStdinBase64(p.Data); err != nil {
		return protocol.TypeContainerExecStdin + ".result", nil, err
	}
	return protocol.TypeContainerExecStdin + ".result", nil, nil
}

func (t *Tunnel) handleContainerExecSignal(id string, payload json.RawMessage) (string, any, error) {
	var p protocol.ContainerExecSignalPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return protocol.TypeContainerExecSignal + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}
	sess, err := t.execs.Get(p.SessionID)
	if err != nil {
		return protocol.TypeContainerExecSignal + ".result", nil, err
	}
	if err := sess.Signal(p.Signal, p.ShellPID); err != nil {
		return protocol.TypeContainerExecSignal + ".result", nil, err
	}
	return protocol.TypeContainerExecSignal + ".result", nil, nil
}

func (t *Tunnel) handleContainerExecKill(id string, payload json.RawMessage) (string, any, error) {
	var p protocol.ContainerExecKillPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return protocol.TypeContainerExecKill + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}
	if err := t.execs.Kill(p.SessionID); err != nil {
		return protocol.TypeContainerExecKill + ".result", nil, err
	}
	return protocol.TypeContainerExecKill + ".result", nil, nil
}

func (t *Tunnel) handleContainerExecCheckAlive(id string, payload json.RawMessage) (string, any, error) {
	var p protocol.ContainerExecKillPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return protocol.TypeContainerExecCheckAlive + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}
	return protocol.TypeContainerExecCheckAlive + ".result", map[string]bool{"alive": t.execs.CheckAlive(p.SessionID)}, nil
}

func (t *Tunnel) handleContainerPause(id string, payload json.RawMessage) (string, any, error) {
	var p protocol.ContainerIDPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return protocol.TypeContainerPause + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}
	if err := t.docker.Pause(p.ContainerID); err != nil {
		return protocol.TypeContainerPause + ".result", nil, err
	}
	return protocol.TypeContainerPause + ".result", map[string]string{"status": "ok"}, nil
}

func (t *Tunnel) handleContainerUnpause(id string, payload json.RawMessage) (string, any, error) {
	var p protocol.ContainerIDPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return protocol.TypeContainerUnpause + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}
	if err := t.docker.Unpause(p.ContainerID); err != nil {
		return protocol.TypeContainerUnpause + ".result", nil, err
	}
	return protocol.TypeContainerUnpause + ".result", map[string]string{"status": "ok"}, nil
}

func (t *Tunnel) handleContainerStop(id string, payload json.RawMessage) (string, any, error) {
	var p protocol.ContainerIDPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return protocol.TypeContainerStop + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}
	if err := t.docker.Stop(p.ContainerID); err != nil {
		return protocol.TypeContainerStop + ".result", nil, err
	}
	return protocol.TypeContainerStop + ".result", map[string]string{"status": "ok"}, nil
}

func (t *Tunnel) handleContainerDestroy(id string, payload json.RawMessage) (string, any, error) {
	var p protocol.ContainerIDPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return protocol.TypeContainerDestroy + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}
	if err := t.docker.Destroy(p.ContainerID); err != nil {
		return protocol.TypeContainerDestroy + ".result", nil, err
	}
	return protocol.TypeContainerDestroy + ".result", map[string]string{"status": "ok"}, nil
}

func (t *Tunnel) handleContainerStatus(id string, payload json.RawMessage) (string, any, error) {
	var p protocol.ContainerIDPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return protocol.TypeContainerStatus + ".result", nil, fmt.Errorf("parse payload: %w", err)
	}
	status, err := t.docker.ContainerStatus(p.ContainerID)
	if err != nil {
		return protocol.TypeContainerStatus + ".result", nil, err
	}
	return protocol.TypeContainerStatus + ".result", map[string]string{"status": status}, nil
}

// --- File message handlers ---

func (t *Tunnel) handleFileRead(id string, payload json.RawMessage) (string, any, error) {
	var p protocol.FileReadPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return protocol.TypeFileReadResult, nil, err
	}
	result, err := fileops.Read(&p)
	if err != nil {
		return protocol.TypeFileReadResult, nil, err
	}
	return protocol.TypeFileReadResult, result, nil
}

func (t *Tunnel) handleFileWrite(id string, payload json.RawMessage) (string, any, error) {
	var p protocol.FileWritePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return protocol.TypeFileWriteResult, nil, err
	}
	result, err := fileops.Write(&p)
	if err != nil {
		return protocol.TypeFileWriteResult, nil, err
	}
	return protocol.TypeFileWriteResult, result, nil
}

func (t *Tunnel) handleFileStat(id string, payload json.RawMessage) (string, any, error) {
	var p protocol.FileStatPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return protocol.TypeFileStatResult, nil, err
	}
	result, err := fileops.Stat(&p)
	if err != nil {
		return protocol.TypeFileStatResult, nil, err
	}
	return protocol.TypeFileStatResult, result, nil
}

func (t *Tunnel) handleFileGlob(id string, payload json.RawMessage) (string, any, error) {
	var p protocol.FileGlobPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return protocol.TypeFileGlobResult, nil, err
	}
	result, err := fileops.Glob(&p)
	if err != nil {
		return protocol.TypeFileGlobResult, nil, err
	}
	return protocol.TypeFileGlobResult, result, nil
}

func (t *Tunnel) handleFileGrep(id string, payload json.RawMessage) (string, any, error) {
	var p protocol.FileGrepPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return protocol.TypeFileGrepResult, nil, err
	}
	result, err := fileops.Grep(&p)
	if err != nil {
		return protocol.TypeFileGrepResult, nil, err
	}
	return protocol.TypeFileGrepResult, result, nil
}

func (t *Tunnel) handleFileMkdir(id string, payload json.RawMessage) (string, any, error) {
	var p protocol.FileMkdirPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return protocol.TypeFileMkdirResult, nil, err
	}
	if err := fileops.Mkdir(&p); err != nil {
		return protocol.TypeFileMkdirResult, nil, err
	}
	return protocol.TypeFileMkdirResult, map[string]string{"status": "ok"}, nil
}

func (t *Tunnel) handleFileRemove(id string, payload json.RawMessage) (string, any, error) {
	var p protocol.FileRemovePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return protocol.TypeFileRemoveResult, nil, err
	}
	if err := fileops.Remove(&p); err != nil {
		return protocol.TypeFileRemoveResult, nil, err
	}
	return protocol.TypeFileRemoveResult, map[string]string{"status": "ok"}, nil
}
