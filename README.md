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
- Generic `proc.*` execution with stdout/stderr streaming, stdin, resize, signal, kill, and liveness checks
- Existing host-side `file.*` operations for Taurus setup and dashboard flows
- Node capacity heartbeat (RAM / CPU / disk)
- File reads from the node data path for dashboard proxying

## Requirements

- Go 1.22+
- For **node** mode on Linux hosts:
  - Taurus is expected to drive any host tooling it needs (for example Docker) through generic `proc.*`

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

- `https://get.taurusagents.com/relay` → `scripts/install.sh`
- `https://get.taurusagents.com/relay.ps1` → `scripts/install.ps1`

Both installers download the latest GitHub release artifact for the current platform,
verify it against `checksums.txt`, install the binary locally, and then run
`taurus-relay connect`.

Because they use GitHub's stable `releases/latest/download/...` URLs, cut a fresh
relay release after merging these installer changes before wiring `get.taurusagents.com`
to them in production.

### Environment variables

- `TAURUS_TOKEN` — one-time registration token from the Taurus UI
- `TAURUS_URL` — Taurus app/control-plane base URL (defaults to `https://app.taurusagents.com`)

Optional advanced overrides:

- `TAURUS_RELAY_VERSION` — exact release tag to install instead of `latest`
- `TAURUS_INSTALL_DIR` — custom install directory
- `TAURUS_RELAY_SKIP_CONNECT=1` — install only, do not immediately run `connect`

### Linux / macOS

```bash
curl -fsSL https://get.taurusagents.com/relay | TAURUS_TOKEN=<registration-token> TAURUS_URL=https://app.taurusagents.com sh
```

### Windows (PowerShell)

```powershell
$env:TAURUS_TOKEN='<registration-token>'; $env:TAURUS_URL='https://app.taurusagents.com'; $installer = Join-Path $env:TEMP 'install-taurus-relay.ps1'; Invoke-WebRequest https://get.taurusagents.com/relay.ps1 -OutFile $installer; powershell -ExecutionPolicy Bypass -File $installer
```

Windows release artifacts are supported for install and `connect` mode. `node` mode is not supported on Windows.

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

Credentials are stored in `~/.config/taurus-relay/config.json`. To store them
somewhere else — for example to run two relay identities from one OS account —
point the relay at a different config directory with `--config-dir` (or the
`TAURUS_RELAY_CONFIG_DIR` environment variable; the flag wins if both are set):

```bash
./taurus-relay connect --config-dir ~/relay-alt \
  --server https://your-taurus-host.example \
  --token <registration-token>
```

The directory always contains a file named `config.json`. `taurus-relay status`
accepts the same `--config-dir` flag.

The number of concurrent sessions (shell and proc) the relay will host is
capped at 128 by default in connect mode. Override it with `--max-sessions <n>`
(or the `TAURUS_RELAY_MAX_SESSIONS` environment variable; the flag wins if both
are set); `0` means unlimited.

### Node mode (Linux hosts)

`node` mode is supported on Linux hosts. Windows releases are connect-only, and `taurus-relay node` exits immediately with an explicit unsupported error on Windows.

```bash
./taurus-relay node \
  --server https://your-taurus-host.example \
  --name node-01 \
  --host 203.0.113.10 \
  --token <node-enrollment-token> \
  --data-path /data/taurus \
  --drive-owner 100000:100000 \
  --max-containers 50
```

Flags:

- `--server` Taurus control plane base URL
- `--name` node name shown in Taurus
- `--host` public IP / hostname of the node
- `--token` node enrollment token
- `--data-path` base path for Taurus drives on the node
- `--drive-owner` **required.** Who owns the agent drive directories the relay creates
  (also settable via `TAURUS_DRIVE_OWNER`; the flag wins). See below.
- `--max-containers` optional node capacity hint
- `--max-sessions` cap on concurrent proc sessions (default: 1048576; `0` = unlimited;
  also settable via `TAURUS_RELAY_MAX_SESSIONS`, the flag wins). The effective cap is
  reported to the control plane at registration.
- `--insecure` allow non-TLS `http://` / `ws://` for local development only

#### Drive ownership under `userns-remap` (`--drive-owner`)

Taurus container nodes run `dockerd` with `userns-remap`, so a container's root user is
a *different host uid* — the remap base, normally `100000:100000`. Anything the relay
creates under an agent's drive would otherwise be owned by the relay's own unix user and
unwritable by the agent inside the container. The relay therefore hands **every directory
and file it creates** under

```text
<data-path>/taurus-drives
<data-path>/taurus-drives-trash
```

to the configured owner — every created component, not just the leaf, because chowning
only the leaf lets the agent write inside it while silently preventing it from creating
siblings. Paths the relay creates elsewhere under `--data-path` (the managed seccomp
profile, for example) are untouched.

`--drive-owner` accepts:

- `<uid>:<gid>` — the userns-remap base. Confirm it with
  `docker info --format '{{.DockerRootDir}}'`, which reads `/var/lib/docker/100000.100000`
  on a remapped daemon (`<uid>.<gid>`).
- `none` — this node does **not** run `dockerd` with `userns-remap`; the relay creates
  drive directories exactly as before and never chowns anything.

There is no default. **Node mode refuses to start when the value is unset or malformed**,
and it refuses to start when it cannot actually apply the ownership: at startup it creates
the two drive roots, verifies they are owned by the configured owner, and creates + chowns
a throwaway `.taurus-drive-owner-check` directory inside each. A node that would create
drive directories the container cannot write never registers with the control plane.

The value is never sniffed from `/etc/subuid` or `docker info`. It is an explicit
deployment input, which the Taurus daemon independently cross-checks against its own
`docker info` userns-remap probe; a node whose published owner disagrees with that probe
is refused for agent launches. The relay publishes the configured value to the control
plane as the `taurus_drive_owner` node metadata key.

> **Upgrade note:** existing nodes that predate `--drive-owner` will refuse to start until
> the flag or `TAURUS_DRIVE_OWNER` is added to their unit. This is intentional — an
> implicit default is exactly the silent misconfiguration this flag exists to prevent.

#### Required privileges for node mode

Applying that ownership needs `CAP_CHOWN` (chown to a foreign uid), `CAP_DAC_OVERRIDE`
(create inside directories owned by the remap base) and `CAP_FOWNER` (chmod/rename them).
Grant them as **file capabilities on the binary**, not as ambient capabilities:

```bash
setcap cap_chown,cap_dac_override,cap_fowner=ep /usr/local/bin/taurus-relay
```

```ini
[Service]
User=taurus
# NoNewPrivileges must stay off. It disables every setuid binary — including the
# sudo that runs `taurus-quota` for XFS project-quota enforcement — and under it
# exec of a file-capability binary fails outright with EPERM, so the relay would
# not start at all (loudly, at least).
NoNewPrivileges=no
# Do NOT add CapabilityBoundingSet: it applies to every descendant and can never
# be raised, so a bounding set of these three capabilities leaves
# `sudo taurus-quota` running as uid 0 without CAP_SYS_ADMIN (needed by
# `xfs_quota -x -c limit`) or CAP_DAC_READ_SEARCH (needed by `du`).
```

Why file capabilities rather than `AmbientCapabilities=CAP_CHOWN CAP_DAC_OVERRIDE CAP_FOWNER`:
ambient capabilities are **inherited by every process the relay exec's** — the docker CLI,
`mv`, `rm`, `cp`, and the shell snippets the daemon sends — which is much wider than "the
relay can chown drive dirs". (They are dropped when exec'ing a setuid or file-capability
binary, so `sudo` does not inherit them; every *plain* binary does.) File capabilities are
not inherited at all.

The cost of file capabilities is that they live in the binary's extended attributes: they
are **lost on every upgrade that replaces the file** (`cp`, `install`, a new release
tarball) and are **inert on a filesystem mounted `nosuid`**. Re-run `setcap` after each
upgrade — the relay's startup self-check turns a forgotten `setcap` into a refused start
rather than a silently mis-owned drive tree, and `getcap /usr/local/bin/taurus-relay`
confirms it.

Ambient capabilities remain a valid alternative if you prefer upgrade-proof configuration
over child isolation; the relay does not care which mechanism supplied its authority.
Running the relay as `root` also works and needs no capability configuration.

## Expected control plane compatibility

The Taurus control plane should expose the relay WebSocket endpoint at:

```text
/api/relay/ws
```

It should support:
- regular relay auth / registration for target mode
- `node.register` auth flow for node mode
- generic `proc.*` and `file.*` RPC for node mode

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
- **macOS**: supported for `connect`; `node` mode is not supported because Taurus container hosting depends on Linux Docker semantics.
- **Windows**: release binaries are built and published, and Windows is supported for install plus `connect` mode. Interactive shell sessions may require the Taurus control plane to request a Windows-appropriate shell (for example `powershell.exe`) instead of assuming `bash`. `node` mode is explicitly unsupported on Windows; Windows releases are connect-only.
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

Public install scripts live in [`scripts/install.sh`](./scripts/install.sh) and [`scripts/install.ps1`](./scripts/install.ps1). If you are wiring up `get.taurusagents.com`, serve or redirect `/relay` and `/relay.ps1` to those script contents.

No license has been added yet.
