# Taurus Relay

Standalone Go relay binary for Taurus.

It connects remote machines to a Taurus control plane over WebSocket and supports two modes:

- **connect** — user relay mode for remote shell/file access
- **node** — Docker node mode for hosting Taurus agent containers on remote machines

## Features

### Relay mode
- Remote interactive shell sessions
- Remote file read/write/stat/glob/grep/mkdir/remove
- Registration-token onboarding + JWT reconnect
- Heartbeat reporting

### Node mode
- Node registration with enrollment token
- Docker container lifecycle:
  - `container.ensure`
  - `container.pause`
  - `container.unpause`
  - `container.stop`
  - `container.destroy`
  - `container.status`
- Streaming `docker exec` sessions for Taurus agent shells
- Node capacity heartbeat (RAM / CPU / disk / running container count)
- File reads from the node data path for dashboard proxying

## Requirements

- Go 1.22+
- For **node** mode:
  - Docker installed and available in `PATH`
  - the relay process user must be able to run `docker`

## Build

```bash
go build -o taurus-relay .
```

Or install directly:

```bash
go install github.com/taurusagents/taurus-relay@latest
```

## Usage

### Show help

```bash
./taurus-relay help
```

### User relay mode

First connection with a one-time registration token:

```bash
./taurus-relay connect \
  --server https://your-taurus-host.example \
  --token <registration-token>
```

Subsequent reconnects reuse saved credentials:

```bash
./taurus-relay connect --server https://your-taurus-host.example
```

### Docker node mode

```bash
./taurus-relay node \
  --server https://your-taurus-host.example \
  --name node-01 \
  --host 203.0.113.10 \
  --token <node-enrollment-token> \
  --data-path /data/taurus \
  --max-containers 50
```

Flags:

- `--server` Taurus control plane base URL
- `--name` node name shown in Taurus
- `--host` public IP / hostname of the node
- `--token` node enrollment token
- `--data-path` base path for Taurus drives on the node
- `--max-containers` optional node capacity hint
- `--insecure` allow non-TLS `http://` / `ws://` for local development only

## Expected control plane compatibility

The Taurus control plane should expose the relay WebSocket endpoint at:

```text
/api/relay/ws
```

It should support:
- regular relay auth / registration for target mode
- `node.register` auth flow for node mode
- `container.*` RPC for node mode

## Development

Build all packages:

```bash
go build ./...
```

Run tests (if/when present):

```bash
go test ./...
```

## Notes

This repository intentionally contains only the relay binary and its internal Go packages. It does **not** include the Taurus control plane / web app / daemon source code.

No license has been added yet.
