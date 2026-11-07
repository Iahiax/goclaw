# Sandbox with udocker (no Docker daemon)

GoClaw runs sandboxed agent commands (`exec`, `read_file`, `write_file`,
`list_files`, `edit`) inside containers. The default container runtime is
[udocker](https://github.com/indigo-dc/udocker): it runs Docker images in user
space with no daemon and no root, so it works on hosts where Docker cannot be
installed (shared servers, HPC, CI runners, Termux/proot, etc.).

Docker is still supported with `GOCLAW_SANDBOX_RUNTIME=docker`.

## Install udocker

```bash
pip install --user udocker      # or: pipx install udocker
udocker install                 # downloads proot/fakechroot/runc tools
udocker version
```

When running as root, GoClaw passes `--allow-root` automatically.

## Build the sandbox image (no Docker needed)

```bash
scripts/udocker-sandbox-image.sh                  # -> goclaw-sandbox:bookworm-slim
```

The script mirrors `Dockerfile.sandbox` (pull `debian:bookworm-slim`, install
packages, export, import). Alternatively use any registry image directly, e.g.
`GOCLAW_SANDBOX_IMAGE=debian:bookworm-slim` (pulled automatically on first use).

## Enable

```bash
export GOCLAW_SANDBOX_MODE=all            # off | non-main | all
export GOCLAW_SANDBOX_RUNTIME=udocker     # default; "docker" to use Docker
export GOCLAW_SANDBOX_IMAGE=goclaw-sandbox:bookworm-slim
./goclaw
```

Or in `config.json` under `agents.defaults.sandbox`:

```json
{ "mode": "all", "runtime": "udocker", "image": "goclaw-sandbox:bookworm-slim" }
```

Optional environment:

| Variable | Purpose |
|----------|---------|
| `GOCLAW_UDOCKER_BIN` | Path to the udocker executable (default `udocker`) |
| `GOCLAW_UDOCKER_EXECMODE` | Execution mode applied to each container (`P1`, `P2`, `F1`..`F4`, `R1`..`R3`, `S1`) |
| `UDOCKER_DIR` | udocker repository location (udocker's own setting, default `~/.udocker`) |

## How it maps

| Docker | udocker |
|--------|---------|
| `docker run -d IMAGE sleep infinity` | `udocker pull` (if missing) + `udocker create --name=NAME IMAGE` |
| `docker exec [-e] [-w] ID cmd` | `udocker run --nobanner --volume=... --env=... --workdir=... ID cmd` |
| `docker rm -f ID` | `udocker rm -f ID` |

udocker has no detached mode, so volumes, env and user are re-applied on every
exec. The container filesystem persists between execs until the sandbox is
released or pruned.

## Limitations

udocker is **not a security boundary**. These settings are not enforced (a
warning is logged once at startup):

- network isolation (`network_enabled=false`)
- `memory_mb`, `cpus`, `pids_limit`
- `read_only_root`, `cap_drop`, `tmpfs`
- read-only mounts (`workspace_access=ro` and runtime read-only mounts are mounted read-write)

Use `GOCLAW_SANDBOX_RUNTIME=docker` when you need those guarantees.

## Test

```bash
GOCLAW_TEST_UDOCKER=1 go test ./internal/sandbox/ -run Udocker -v
GOCLAW_TEST_UDOCKER=1 GOCLAW_TEST_UDOCKER_IMAGE=goclaw-sandbox:bookworm-slim go test ./internal/sandbox/ -run Integration -v
```
