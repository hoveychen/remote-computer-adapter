# remote-computer-adapter (`rca`)

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
rca codex-native  --config ~/.config/rca/codex.json  -- exec "fix the failing test"
rca claude-native --config ~/.config/rca/claude.json -- "fix the failing test"
```

The two engines reach that boundary differently, and the difference is worth
knowing before you pick one.

**Codex is native.** Its memory and skills backends are injectable, so the
trusted service sits behind them and the model keeps calling the tools it
always had, under their own names, with their own UX. Its file and exec tools
keep working too; only the execution environment they bind to changes.

**Claude Code is contained.** It is a closed binary with no state-backend seam,
so its built-in tools are removed outright (`--tools ""`, measured) and
replaced by rca's own. The model sees `mcp__rca__` tool names and does not get
native memory, skills or slash commands. That distribution shift is a real
cost; it is accepted because the alternative is no boundary at all.

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
| A Codex build carrying the native-state patches | upstream Codex plus the patches that let native memory/skills call the trusted service instead of `$CODEX_HOME`. Only needed for `codex-native` |
| A stock `claude` | any recent Claude Code. Only needed for `claude-native`; no patches, because there is nothing to patch into |
| A remote executor | a host reachable by a transport program. Codex talks to `codex exec-server --listen stdio`; Claude talks to `rca serve --root <dir>` |

The transport is whatever program you name — typically `ssh`. `rca` execs it with
a cleared environment, so harness credentials never reach the remote side.

## Install

Grab the single binary from
[Releases](https://github.com/hoveychen/remote-computer-adapter/releases):

```sh
# macOS (Apple silicon)
curl -fsSL https://github.com/hoveychen/remote-computer-adapter/releases/latest/download/rca_darwin_arm64.tar.gz | tar xz
# macOS (Intel)
curl -fsSL https://github.com/hoveychen/remote-computer-adapter/releases/latest/download/rca_darwin_amd64.tar.gz | tar xz
# Linux (x86_64)
curl -fsSL https://github.com/hoveychen/remote-computer-adapter/releases/latest/download/rca_linux_amd64.tar.gz | tar xz
# Linux (arm64)
curl -fsSL https://github.com/hoveychen/remote-computer-adapter/releases/latest/download/rca_linux_arm64.tar.gz | tar xz

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

The Claude config has the same shape, with `remote_root` in place of
`remote_cwd` and an executor that is `rca` itself:

```json
{
  "binary": "/usr/local/bin/claude",
  "runtime_home": "/Users/you/.local/state/rca/claude-runtime",
  "state_root": "/Users/you/.local/state/rca/store",
  "remote_root": "/work/project",
  "exec_program": "/usr/bin/ssh",
  "exec_args": ["-T", "-o", "ForwardAgent=no", "sandbox-host",
                "rca serve --root /work/project"]
}
```

`remote_root` must match what the executor reports at startup, or the session
refuses to begin — a harness pointed at a different tree than the operator
configured is a silent mismatch, not a workable default.

### 2. Run

```sh
rca codex-native --config ~/.config/rca/native.json -- exec "fix the failing test"
rca codex-native --config ~/.config/rca/native.json -- exec --json -- "summarise the diff"
```

Only `exec` is supported, with `--json` and `--color`. Config, profile and
resume overrides are rejected — they are exactly the arguments that could
reintroduce a local execution domain.

For Claude, the command line is one prompt and nothing else:

```sh
rca claude-native --config ~/.config/rca/claude.json -- "fix the failing test"
```

`--tools`, `--mcp-config`, `--settings` and `--dangerously-skip-permissions`
are refused for the same reason: each would reopen something the trusted side
is supposed to own.

## How it works

### Codex — the native path

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

### Claude — the contained path

```
    ┌──────────────────── trusted machine ────────────────────┐
    │                                                          │
    │  rca (Go)                                                │
    │   ├─ private 0700 runtime home it locks and owns:        │
    │   │  HOME and CLAUDE_CONFIG_DIR both point inside it     │
    │   ├─ trusted state service (loopback, bearer token)      │
    │   │   /mcp  the session's ONLY tool surface:             │
    │   │     workspace_read/write/list/search/exec → executor │
    │   │     memory_*, skills_*                     → journal │
    │   └─ launches  claude --tools "" --restricted            │
    │                       --strict-mcp-config                │
    │                              │                           │
    │        no built-in tools exist to bind anywhere else      │
    └──────────────────────────────┼───────────────────────────┘
                                   │  exec ssh (env cleared)
                                   ▼
              ┌────────── untrusted executor ──────────┐
              │  rca serve --root <dir>                 │
              │   confined to one root; every path      │
              │   checked after symlink resolution      │
              └─────────────────────────────────────────┘
```

The two tool families are addressed differently on purpose. `workspace_*` takes
paths relative to the executor's root and has no host backend to name;
`memory_*` and `skills_*` take logical IDs and never paths. A single tool that
chose its backend from the path would put that choice in the model's hands.

## Verified status

### Codex

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

### Claude

Measured on Claude Code 2.1.263, on this branch:

| What | Result |
|---|---|
| Tool surface (`scripts/test-claude-toolface.py`) | 21/21 |
| Isolation e2e (`scripts/test-claude-native.py`) | 26/26 |

The tool-surface probe is the load-bearing one. It points a real claude at a
loopback Messages API and records what the harness advertised and executed, so
the result does not depend on a model choosing to obey a prompt: with
`--tools ""` the advertised set is exactly the registered MCP tools, and an
unregistered `Bash` call comes back as `No such tool available` with no side
effect. Its control case shows `--restricted` alone is *not* enough — the
built-in file tools survive it — so `--tools` is the flag doing the work.

The e2e then drives the whole path: reads, searches, writes and commands land
in the remote root; an absolute host path and a `../` traversal are refused; no
harness token reaches the executor's environment; the trusted journal holds one
commit, records the revision conflict with its reason, and grows no entry for a
replayed request id.

What is **not** claimed: Claude Code is closed, so its internal write paths
cannot be enumerated. rca contains them — `HOME` and `CLAUDE_CONFIG_DIR` point
into a private runtime home it locks and owns — rather than mediating them, and
it scans that home after each session for state roots (memory, skills, plugins,
hooks) that would bypass the trusted store, warning if any appear. Managed
policy settings still apply, by design: the operator is the administrator.

The full analysis of why the native half is unavailable here is in
[`docs/tool-boundary-implementation.md`](docs/tool-boundary-implementation.md).

## Development

```sh
make                      # rca into ./bin
make test                 # go test ./...
scripts/test-codex-native.py --help    # codex isolation e2e (needs Docker + a patched codex)
scripts/test-claude-toolface.py        # what claude really advertises to the model
scripts/test-claude-native.py          # claude isolation e2e (needs a claude binary)
scripts/build-release.sh  # all four release archives into ./dist
```

Design history lives in [`docs/`](docs/): the protocol in
`codex-go-native-state-protocol.md`, the source-level integration study in
`codex-native-state-integration.md`, and the tool-boundary evidence in
`tool-boundary-implementation.md`.

## License

MIT — see [LICENSE](LICENSE).
