# remote-adapter (`rca`)

A **trusted harness** for coding agents. The agent's conversation, credentials,
memory and skills stay on a machine you control; every general file and
subprocess tool it can call reaches **only an untrusted third-party executor**,
with no local fallback to fall back to.

Two boundaries, enforced by construction rather than by prompt:

- **General tools are remote-only.** `rca` writes the harness config itself into
  a private, managed home directory and registers exactly one execution
  environment — the remote one. There is no `local` environment ID for the model
  to name, and the harness is launched with `--strict-config` so nothing it reads
  later can add one.
- **Semantic state is transactional.** Memory notes, background extraction and
  consolidation, and skill packages all commit through a Go state service that
  owns the only writable copy. Every mutation carries an actor identity the model
  cannot forge, a CAS revision, and an idempotent request id, and lands in a
  single append-only journal entry together with its audit record.

```sh
rca codex-native --config ~/.config/rca/native.json -- exec "fix the failing test"
```

## What this is not

- It is **not** an information-flow guarantee. The model reads your private
  history and then chooses what to run remotely; that choice is itself a
  channel. See [`docs/strict-information-flow-design.md`](docs/strict-information-flow-design.md)
  for what a strict non-interference design would cost, and why it was not taken.
- The audit covers the **semantic state** it claims — memory content, memory job
  watermarks, skill package revisions and tombstones. It is not a formal audit of
  every syscall the harness process makes. Session SQLite, auth and plugin
  storage keep their own existing locations.
- The safety of the general tool surface depends on the executor actually being
  a separate machine or a container with no trusted mount. `rca` binds the
  backend; it cannot vouch for the remote host.

## Requirements

| Piece | What it is |
|---|---|
| `rca` | this repo; pure Go, no cgo, single binary |
| A Codex build carrying the native-state patches | upstream Codex plus the patches that let native memory/skills call the trusted service instead of `$CODEX_HOME` |
| A remote executor | any host reachable by a transport program that can run `codex exec-server --listen stdio` |

The transport is whatever program you name — typically `ssh`. `rca` execs it with
a cleared environment, so harness credentials never reach the remote side.

## Install

Grab the single binary from
[Releases](https://github.com/hoveychen/remote-adapter/releases):

```sh
# macOS (Apple silicon)
curl -fsSL https://github.com/hoveychen/remote-adapter/releases/latest/download/rca_darwin_arm64.tar.gz | tar xz
# macOS (Intel)
curl -fsSL https://github.com/hoveychen/remote-adapter/releases/latest/download/rca_darwin_amd64.tar.gz | tar xz
# Linux (x86_64)
curl -fsSL https://github.com/hoveychen/remote-adapter/releases/latest/download/rca_linux_amd64.tar.gz | tar xz
# Linux (arm64)
curl -fsSL https://github.com/hoveychen/remote-adapter/releases/latest/download/rca_linux_arm64.tar.gz | tar xz

sudo install -m 755 rca /usr/local/bin/rca
rca version
```

`checksums.txt` on each release carries the sha256 of every archive.

Or build from source (Go 1.25+):

```sh
make            # rca into ./bin
```

## Usage

### 1. Write a config

Every path is absolute. `runtime_home` must be a new directory or one `rca`
already owns — it refuses to adopt an unmanaged non-empty one, and holds an
exclusive lock on it while running.

```json
{
  "binary": "/opt/codex-native/bin/codex",
  "runtime_home": "/Users/you/.local/state/rca/runtime",
  "state_root": "/Users/you/.local/state/rca/store",
  "remote_cwd": "/work/project",
  "exec_program": "/usr/bin/ssh",
  "exec_args": ["-T", "-o", "ForwardAgent=no", "sandbox-host",
                "codex exec-server --listen stdio"]
}
```

`remote_cwd` is the working directory **on the executor**. `exec_args` is fixed
by you, not selectable by the model.

### 2. Run

```sh
rca codex-native --config ~/.config/rca/native.json -- exec "fix the failing test"
rca codex-native --config ~/.config/rca/native.json -- exec --json -- "summarise the diff"
```

Only `exec` is supported, with `--json` and `--color`. Config, profile and
resume overrides are rejected — they are exactly the arguments that could
reintroduce a local execution domain.

## How it works

```
    ┌──────────────────── trusted machine ────────────────────┐
    │                                                          │
    │  rca (Go)                                                │
    │   ├─ writes config.toml + environments.toml into a       │
    │   │  private 0700 CODEX_HOME it locks and owns           │
    │   ├─ trusted state service (loopback, per-actor tokens)  │
    │   │   /mcp        memory + skills semantic tools         │
    │   │   /native/v2  native memory/skills backend           │
    │   │        └─ append-only journal: content + revision    │
    │   │           + audit + job watermark, one fsync'd entry │
    │   └─ drives  codex app-server --strict-config  over stdio│
    │                              │                           │
    │        general file/exec tools bind to one environment    │
    └──────────────────────────────┼───────────────────────────┘
                                   │  rca _native-transport
                                   │  (env cleared, exec ssh)
                                   ▼
              ┌────────── untrusted executor ──────────┐
              │  codex exec-server --listen stdio       │
              │   process start/read, filesystem ops    │
              └─────────────────────────────────────────┘
```

The three actor tokens are separate: `model_tool` (bound to the app-server's
real thread id, only after it reports one), `background` (extraction and
consolidation jobs) and `installer` (bundled skill packages). A model tool call
cannot present a background token, and none of them accepts a host path.

## Verified status

Last full acceptance: 2026-09-07, against upstream Codex `5ecb3afd1b` plus the
native-state patches, with the executor running in Docker with `--network none`
and no host mount.

| Layer | Result |
|---|---|
| `go test -count=1 ./...`, plus `-race` | PASS |
| Rust `codex-memories-extension` + `codex-memories-write` | 73/73 |
| Rust `codex-skills` + `codex-skills-extension` | 229/229 |
| Rust `codex-app-server native_`, `codex-core native_memory` | 7/7 |
| Isolation e2e (`scripts/test-codex-native.py`) | 28/28 |

Observed in that run: the executor could not read the trusted sentinel or
journal, no harness token reached its environment, the trusted runtime created
no local `memories/` or `skills/` fallback directory, and revision conflicts,
replayed request ids, path traversal, host-authority rewrites, an offline
executor and a malformed exec-server were all rejected. Details and the
explicit limits of that claim are in
[`docs/codex-native-state-latest-validation.md`](docs/codex-native-state-latest-validation.md).

Claude Code support is not implemented. Because it is a closed-source binary
with no state-backend injection point, it cannot get the *native* half of this
design; the analysis and the containment-based alternative are in
[`docs/tool-boundary-implementation.md`](docs/tool-boundary-implementation.md).

## Development

```sh
make                      # rca into ./bin
make test                 # go test ./...
scripts/test-codex-native.py --help    # isolation e2e (needs Docker + a patched codex)
scripts/build-release.sh  # all four release archives into ./dist
```

Design history lives in [`docs/`](docs/): the protocol in
`codex-go-native-state-protocol.md`, the source-level integration study in
`codex-native-state-integration.md`, and the tool-boundary evidence in
`tool-boundary-implementation.md`.

## License

MIT — see [LICENSE](LICENSE).
