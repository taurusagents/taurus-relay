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

## Quick install via public scripts

These are the public bootstrap scripts intended to be served at:

- `https://get.taurus.cloud/relay` → `scripts/install.sh`
- `https://get.taurus.cloud/relay.ps1` → `scripts/install.ps1`

Both installers download the latest GitHub release artifact for the current platform,
verify it against `checksums.txt`, install the binary locally, and then run
`taurus-relay connect`.

Because they use GitHub's stable `releases/latest/download/...` URLs, cut a fresh
relay release after merging these installer changes before wiring `get.taurus.cloud`
to them in production.

### Environment variables

- `TAURUS_TOKEN` — one-time registration token from the Taurus UI
- `TAURUS_URL` — Taurus app/control-plane base URL (defaults to `https://app.taurus.cloud`)

Optional advanced overrides:

- `TAURUS_RELAY_VERSION` — exact release tag to install instead of `latest`
- `TAURUS_INSTALL_DIR` — custom install directory
- `TAURUS_RELAY_SKIP_CONNECT=1` — install only, do not immediately run `connect`

### Linux / macOS

```bash
curl -fsSL https://get.taurus.cloud/relay | TAURUS_TOKEN=<registration-token> TAURUS_URL=https://app.taurus.cloud sh
```

### Windows (PowerShell)

```powershell
$env:TAURUS_TOKEN='<registration-token>'; $env:TAURUS_URL='https://app.taurus.cloud'; $installer = Join-Path $env:TEMP 'install-taurus-relay.ps1'; Invoke-WebRequest https://get.taurus.cloud/relay.ps1 -OutFile $installer; powershell -ExecutionPolicy Bypass -File $installer
```

For self-hosted Taurus, replace `TAURUS_URL` with your public Taurus app URL.

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

If you installed via the public bootstrap script, the installer runs this command for
you after downloading the correct release binary.

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
- The public installers depend on the archive naming above staying stable as `taurus-relay_<version>_<os>_<arch>.(tar.gz|zip)` plus `checksums.txt`; `.goreleaser.yaml` now pins that explicitly.

### How to cut a release

Preferred one-command flow:

```bash
./scripts/release minor
```

That helper will:

1. verify you are on a clean `main`
2. fetch `origin/main` and tags
3. fast-forward local `main` if needed
4. push local `main` first if it is ahead of origin
5. detect the latest stable `vX.Y.Z` tag
6. bump the requested semver segment (`major`, `minor`, or `patch`)
7. create an annotated tag
8. push the tag to `origin`, triggering the Release workflow

Examples:

```bash
./scripts/release minor
./scripts/release patch
./scripts/release major --dry-run
```

If there are no existing release tags yet, the helper starts from:

- `minor` → `v0.1.0`
- `patch` → `v0.0.1`
- `major` → `v1.0.0`

You can still push a tag manually if needed:

```bash
git tag v0.1.0
git push origin v0.1.0
```

After pushing the tag, wait for the **Release** workflow to finish and verify artifacts plus `checksums.txt` on the GitHub Release page.

Optional local GoReleaser dry-run:

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

Public install scripts live in [`scripts/install.sh`](./scripts/install.sh) and [`scripts/install.ps1`](./scripts/install.ps1). If you are wiring up `get.taurus.cloud`, serve or redirect `/relay` and `/relay.ps1` to those script contents.

No license has been added yet.
