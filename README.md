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

Run tests:

```bash
go test ./...
```

## CI/CD and releases

This repo ships with two GitHub Actions workflows:

- **CI** (`.github/workflows/ci.yml`) on pushes to `main` and pull requests:
  - `go test ./...`
  - cross-compile checks for `linux/darwin/windows` on `amd64/arm64`
- **Release** (`.github/workflows/release.yml`) on tags matching `v*`:
  - runs GoReleaser
  - publishes a GitHub Release with archives + checksums

### Release artifacts

GoReleaser (`.goreleaser.yaml`) builds `taurus-relay` for:

- linux/amd64
- linux/arm64
- darwin/amd64
- darwin/arm64
- windows/amd64
- windows/arm64

Current support notes:

- **Linux**: fully supported for both `connect` and `node` mode.
- **macOS**: supported for `connect`; `node` mode is not a normal deployment target because it depends on Docker-based Taurus container hosting on Linux.
- **Windows**: release binaries are built and published, but interactive `connect` sessions may require an explicit shell (for example `powershell.exe`) instead of assuming `bash`. The current Taurus control plane commonly requests `bash` for relay shell sessions, so native Windows `connect` support should still be treated as provisional until the control plane can request a Windows-appropriate shell. `node` mode should also be treated as experimental unless/until it is validated end-to-end on native Windows hosts.

### How to cut a release

1. Merge your changes to `main`.
2. Create and push a semantic version tag:

```bash
git tag v0.1.0
git push origin v0.1.0
```

3. Wait for the **Release** workflow to finish.
4. Verify artifacts and `checksums.txt` in the GitHub Release page.

Optional local dry-run:

```bash
goreleaser release --snapshot --clean
```

### Required GitHub settings

In the GitHub repository settings:

- **Actions** must be enabled.
- Workflows must be allowed to create releases with `contents: write` permission.
  - If your org/repo policy restricts token permissions, allow **Read and write** workflow permissions for this repo.

## Notes

This repository intentionally contains only the relay binary and its internal Go packages. It does **not** include the Taurus control plane / web app / daemon source code.

No license has been added yet.
