// Package protocol defines the JSON message types for Relay ↔ Daemon communication.
package protocol

import "encoding/json"

// Message is the envelope for all WebSocket communication.
type Message struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	Error    *string         `json:"error,omitempty"`
	Priority string          `json:"priority,omitempty"`
	// Generation is relay-local metadata used to bind already-read requests to the
	// runtime generation that received them. It is never serialized on the wire.
	Generation uint64 `json:"-"`
}

// ErrorString returns a non-nil error string pointer.
func ErrorString(s string) *string { return &s }

const (
	PriorityNormal   = "normal"
	PriorityPriority = "priority"
)

// NormalizePriority collapses unknown values back to the default normal lane.
func NormalizePriority(priority string) string {
	if priority == PriorityPriority {
		return PriorityPriority
	}
	return PriorityNormal
}

// --- Shell message payloads ---

type ShellCreatePayload struct {
	SessionID string            `json:"session_id"`
	Shell     string            `json:"shell,omitempty"`
	Args      []string          `json:"args,omitempty"`
	CWD       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

type ShellExecPayload struct {
	SessionID string `json:"session_id"`
	Command   string `json:"command"`
	Timeout   int    `json:"timeout,omitempty"` // ms, 0 = default (120s)
}

type ShellExecResultPayload struct {
	SessionID  string `json:"session_id"`
	Stdout     string `json:"stdout"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int64  `json:"duration_ms"`
}

type ShellWriteStdinPayload struct {
	SessionID string `json:"session_id"`
	Data      string `json:"data"` // base64
}

type ShellResizePayload struct {
	SessionID string `json:"session_id"`
	Cols      uint16 `json:"cols"`
	Rows      uint16 `json:"rows"`
}

type ShellKillPayload struct {
	SessionID string `json:"session_id"`
}

type ShellSignalPayload struct {
	SessionID string `json:"session_id"`
	Signal    string `json:"signal"` // SIGINT, SIGTERM, SIGKILL
}

type ShellOutputPayload struct {
	SessionID string `json:"session_id"`
	Data      string `json:"data"` // base64
}

type ShellExitPayload struct {
	SessionID string `json:"session_id"`
	ExitCode  int    `json:"exit_code"`
}

// --- Proc message payloads ---

type ProcRunPayload struct {
	// RunID lets Taurus provide a stable cancel handle without coupling proc.cancel
	// to the outer transport envelope's message ID.
	RunID     string            `json:"run_id,omitempty"`
	Argv      []string          `json:"argv"`
	CWD       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Stdin     string            `json:"stdin,omitempty"`
	TimeoutMs int               `json:"timeout_ms,omitempty"`
}

type ProcRunResultPayload struct {
	// Stdout/Stderr are convenience text fields populated only when the captured
	// bytes are valid UTF-8. Use StdoutBase64/StderrBase64 for exact bytes.
	Stdout       string `json:"stdout"`
	StdoutBase64 string `json:"stdout_b64,omitempty"`
	Stderr       string `json:"stderr"`
	StderrBase64 string `json:"stderr_b64,omitempty"`
	ExitCode     int    `json:"exit_code"`
	DurationMs   int64  `json:"duration_ms"`
	TimedOut     bool   `json:"timed_out,omitempty"`
	Canceled     bool   `json:"canceled,omitempty"`
}

type ProcCancelPayload struct {
	RunID string `json:"run_id,omitempty"`
	// RequestID remains as a narrow transitional fallback for older Taurus calls
	// that still key one-shot cancellation off the transport envelope ID.
	RequestID string `json:"request_id,omitempty"`
}

type ProcSpawnPayload struct {
	SessionID string            `json:"session_id"`
	Argv      []string          `json:"argv"`
	CWD       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Pty       bool              `json:"pty,omitempty"`
	Cols      uint16            `json:"cols,omitempty"`
	Rows      uint16            `json:"rows,omitempty"`
}

type ProcStdinPayload struct {
	SessionID string `json:"session_id"`
	Data      string `json:"data"`
}

type ProcResizePayload struct {
	SessionID string `json:"session_id"`
	Cols      uint16 `json:"cols"`
	Rows      uint16 `json:"rows"`
}

type ProcSignalPayload struct {
	SessionID string `json:"session_id"`
	Signal    string `json:"signal"`
}

type ProcKillPayload struct {
	SessionID string `json:"session_id"`
}

type ProcOutputPayload struct {
	SessionID string `json:"session_id"`
	Stream    string `json:"stream"`
	Data      string `json:"data"`
}

type ProcExitPayload struct {
	SessionID string `json:"session_id"`
	ExitCode  int    `json:"exit_code"`
}

// --- File message payloads ---

type FileReadPayload struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

type FileReadResultPayload struct {
	Content string `json:"content"` // base64
	Size    int64  `json:"size"`
}

type FileWritePayload struct {
	Path    string `json:"path"`
	Content string `json:"content"` // base64
	Mode    uint32 `json:"mode,omitempty"`
}

type FileWriteResultPayload struct {
	BytesWritten int `json:"bytes_written"`
}

type FileStatPayload struct {
	Path string `json:"path"`
}

type FileStatResultPayload struct {
	Size  int64  `json:"size"`
	Mode  uint32 `json:"mode"`
	Mtime string `json:"mtime"` // ISO 8601
	IsDir bool   `json:"is_dir"`
}

type FileGlobPayload struct {
	Pattern string `json:"pattern"`
	CWD     string `json:"cwd,omitempty"`
}

type FileGlobResultPayload struct {
	Paths []string `json:"paths"`
}

type FileGrepPayload struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
	Glob    string `json:"glob,omitempty"`
}

type GrepMatch struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

type FileGrepResultPayload struct {
	Matches []GrepMatch `json:"matches"`
}

type FileMkdirPayload struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

type FileRemovePayload struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

// --- Node and container payloads ---

type NodeRegisterPayload struct {
	Type             string            `json:"type"`
	Name             string            `json:"name"`
	Host             string            `json:"host"`
	EnrollmentToken  string            `json:"enrollment_token"`
	AllocatableRAMGB float64           `json:"allocatable_ram_gb"`
	AllocatableCPUs  int               `json:"allocatable_cpus"`
	MaxContainers    int               `json:"max_containers,omitempty"`
	Capabilities     map[string]bool   `json:"capabilities,omitempty"`
	Meta             map[string]string `json:"meta,omitempty"`
}

type NodeRegisterResultPayload struct {
	OK     bool   `json:"ok"`
	NodeID string `json:"node_id,omitempty"`
	Error  string `json:"error,omitempty"`
}

type ContainerEnsurePayload struct {
	ContainerID    string               `json:"container_id"`
	Image          string               `json:"image,omitempty"`
	DockerImage    string               `json:"docker_image,omitempty"`
	UserID         string               `json:"user_id"`
	AgentID        string               `json:"agent_id"`
	RootAgentID    string               `json:"root_agent_id"`
	ResourceLimits DockerResourceLimits `json:"resource_limits,omitempty"`
	Mounts         []DockerMount        `json:"mounts,omitempty"`
}

type DockerResourceLimits struct {
	CPUs      float64 `json:"cpus,omitempty"`
	MemoryMB  int     `json:"memory_mb,omitempty"`
	PidsLimit int     `json:"pids_limit,omitempty"`
}

type DockerMount struct {
	Host      string `json:"host"`
	Container string `json:"container"`
	Readonly  bool   `json:"readonly,omitempty"`
}

type ContainerExecPayload struct {
	ContainerID string            `json:"container_id"`
	SessionID   string            `json:"session_id"`
	Command     string            `json:"command"`
	Args        []string          `json:"args,omitempty"`
	CWD         string            `json:"cwd,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Tty         bool              `json:"tty,omitempty"`
	Cols        uint16            `json:"cols,omitempty"`
	Rows        uint16            `json:"rows,omitempty"`
	Stream      bool              `json:"stream,omitempty"`
}

type ContainerExecStdinPayload struct {
	ContainerID string `json:"container_id"`
	SessionID   string `json:"session_id"`
	Data        string `json:"data"`
}

type ContainerExecResizePayload struct {
	SessionID string `json:"session_id"`
	Cols      uint16 `json:"cols"`
	Rows      uint16 `json:"rows"`
}

type ContainerExecSignalPayload struct {
	ContainerID string `json:"container_id"`
	SessionID   string `json:"session_id"`
	Signal      string `json:"signal"`
	ShellPID    int    `json:"shell_pid,omitempty"`
}

type ContainerExecKillPayload struct {
	ContainerID string `json:"container_id"`
	SessionID   string `json:"session_id"`
}

type ContainerExecCommandPayload struct {
	ContainerID string   `json:"container_id"`
	Command     []string `json:"command"`
}

type ContainerExecWithStdinPayload struct {
	ContainerID string   `json:"container_id"`
	Command     []string `json:"command"`
	Stdin       string   `json:"stdin"`
}

type ContainerIDPayload struct {
	ContainerID string `json:"container_id"`
}

// --- Auth payloads ---

// AuthRegistrationPayload is sent inside `type: "auth"` when registering with a one-time token.
type AuthRegistrationPayload struct {
	RegistrationToken string `json:"registration_token"`
	Hostname          string `json:"hostname"`
	OS                string `json:"os"`
	Arch              string `json:"arch"`
	RelayVersion      string `json:"relay_version,omitempty"`
}

// AuthReconnectPayload is sent inside `type: "auth"` when reconnecting with a saved JWT.
type AuthReconnectPayload struct {
	JWT string `json:"jwt"`
}

// AuthResultPayload is the response from the server's `auth.result` message.
type AuthResultPayload struct {
	OK       bool   `json:"ok"`
	TargetID string `json:"target_id,omitempty"`
	JWT      string `json:"jwt,omitempty"`
	Error    string `json:"error,omitempty"`
}

// --- Health payloads ---

type HeartbeatPayload struct {
	OS                string  `json:"os"`
	Arch              string  `json:"arch"`
	Hostname          string  `json:"hostname"`
	Uptime            int64   `json:"uptime"`
	CPUPercent        float64 `json:"cpu_percent"`
	MemoryPercent     float64 `json:"memory_percent"`
	RelayVersion      string  `json:"relay_version"`
	Sessions          int     `json:"sessions"`
	ContainerCount    int     `json:"container_count,omitempty"`
	MemoryUsedGB      float64 `json:"memory_used_gb,omitempty"`
	MemoryAvailableGB float64 `json:"memory_available_gb,omitempty"`
	CPULoad           float64 `json:"cpu_load,omitempty"`
	DiskUsedGB        float64 `json:"disk_used_gb,omitempty"`
	DiskAvailableGB   float64 `json:"disk_available_gb,omitempty"`
}

// Message type constants.
const (
	TypeShellCreate     = "shell.create"
	TypeShellExec       = "shell.exec"
	TypeShellExecResult = "shell.exec.result"
	TypeShellWriteStdin = "shell.write_stdin"
	TypeShellResize     = "shell.resize"
	TypeShellKill       = "shell.kill"
	TypeShellSignal     = "shell.signal"
	TypeShellOutput     = "shell.output"
	TypeShellExit       = "shell.exit"

	TypeProcRun              = "proc.run"
	TypeProcRunResult        = "proc.run.result"
	TypeProcCancel           = "proc.cancel"
	TypeProcCancelResult     = "proc.cancel.result"
	TypeProcSpawn            = "proc.spawn"
	TypeProcSpawnResult      = "proc.spawn.result"
	TypeProcOutput           = "proc.output"
	TypeProcExit             = "proc.exit"
	TypeProcStdin            = "proc.stdin"
	TypeProcStdinResult      = "proc.stdin.result"
	TypeProcResize           = "proc.resize"
	TypeProcResizeResult     = "proc.resize.result"
	TypeProcSignal           = "proc.signal"
	TypeProcSignalResult     = "proc.signal.result"
	TypeProcKill             = "proc.kill"
	TypeProcKillResult       = "proc.kill.result"
	TypeProcCheckAlive       = "proc.check_alive"
	TypeProcCheckAliveResult = "proc.check_alive.result"

	TypeFileRead         = "file.read"
	TypeFileReadResult   = "file.read.result"
	TypeFileWrite        = "file.write"
	TypeFileWriteResult  = "file.write.result"
	TypeFileStat         = "file.stat"
	TypeFileStatResult   = "file.stat.result"
	TypeFileGlob         = "file.glob"
	TypeFileGlobResult   = "file.glob.result"
	TypeFileGrep         = "file.grep"
	TypeFileGrepResult   = "file.grep.result"
	TypeFileMkdir        = "file.mkdir"
	TypeFileMkdirResult  = "file.mkdir.result"
	TypeFileRemove       = "file.remove"
	TypeFileRemoveResult = "file.remove.result"

	TypeAuth               = "auth"
	TypeAuthResult         = "auth.result"
	TypeNodeRegister       = "node.register"
	TypeNodeRegisterResult = "node.register.result"

	TypeContainerEnsure         = "container.ensure"
	TypeContainerExec           = "container.exec"
	TypeContainerExecOutput     = "container.exec.output"
	TypeContainerExecExit       = "container.exec.exit"
	TypeContainerExecStdin      = "container.exec.stdin"
	TypeContainerExecResize     = "container.exec.resize"
	TypeContainerExecSignal     = "container.exec.signal"
	TypeContainerExecKill       = "container.exec.kill"
	TypeContainerExecCheckAlive = "container.exec.check_alive"
	TypeContainerPause          = "container.pause"
	TypeContainerUnpause        = "container.unpause"
	TypeContainerStop           = "container.stop"
	TypeContainerDestroy        = "container.destroy"
	TypeContainerStatus         = "container.status"
	TypeContainerExecCommand    = "container.exec_command"
	TypeContainerExecWithStdin  = "container.exec_with_stdin"

	TypeHeartbeat = "heartbeat"
	TypePing      = "ping"
	TypePong      = "pong"
)
