#!/usr/bin/env python3
"""Isolation e2e for `rca claude-native`.

A real claude binary, a real rca serve executor, a real trusted state store,
and a scripted Messages API on loopback. No Anthropic account, no network, no
model choice: every assertion is about what the harness advertised, what it
executed, and where the bytes landed.

The scripted model deliberately reaches for things it must not have — a host
path, a built-in tool, a stale revision — because a boundary is only shown by
what it refuses.

Requires: a claude binary (--claude) and a built rca (--rca).
"""
import argparse
import json
import os
import pathlib
import shutil
import subprocess
import tempfile
import threading
import http.server

HERE = pathlib.Path(__file__).resolve().parent


def load_toolface():
    """Reuse the recorder and SSE encoder from the tool-surface probe."""
    import importlib.util
    spec = importlib.util.spec_from_file_location("tf", HERE / "test-claude-toolface.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class Checks:
    def __init__(self):
        self.results = []

    def check(self, name, ok, detail=""):
        self.results.append((name, bool(ok), detail))

    def report(self):
        failed = 0
        for name, ok, detail in self.results:
            print(f"{'PASS' if ok else 'FAIL'} {name}" + (f"  {detail}" if detail and not ok else ""))
            failed += not ok
        print(f"\n{len(self.results) - failed}/{len(self.results)} assertions passed")
        return failed


def build_world(root):
    """Lay out the remote workspace and the host-only files it must not reach."""
    remote = root / "remote"
    (remote / "src").mkdir(parents=True)
    (remote / "src" / "app.go").write_text("package main // NEEDLE\n")
    (remote / "README.md").write_text("remote workspace\n")

    # Host-side canaries. Nothing the model can name should reach these.
    host_secret = root / "HOST_SECRET"
    host_secret.write_text("HOST_ONLY_CANARY")
    return remote, host_secret


def script(root, remote, host_secret):
    """The turn sequence. Each entry is (assistant blocks, stop_reason)."""
    def tool(tid, name, args):
        return ([{"type": "tool_use", "id": tid, "name": name, "input": args}], "tool_use")

    return [
        tool("t1", "mcp__rca__workspace_read", {"path": "src/app.go"}),
        tool("t2", "mcp__rca__workspace_search", {"pattern": "NEEDLE"}),
        tool("t3", "mcp__rca__workspace_write", {"path": "out/written.txt", "content": "FROM_MODEL"}),
        tool("t4", "mcp__rca__workspace_exec", {"argv": ["/bin/sh", "-c", "pwd; printf %s \"$RCA_STATE_TOKEN\""]}),
        # Host path, absolute: there is no host backend behind this tool.
        tool("t5", "mcp__rca__workspace_read", {"path": str(host_secret)}),
        # Traversal out of the remote root.
        tool("t6", "mcp__rca__workspace_read", {"path": "../HOST_SECRET"}),
        # Trusted state: a create, then a stale-revision write of the same id.
        tool("t7", "mcp__rca__memory_put", {"id": "e2e-note", "content": "trusted",
                                            "expected_revision": 0, "request_id": "e2e-1"}),
        tool("t8", "mcp__rca__memory_put", {"id": "e2e-note", "content": "clobber",
                                            "expected_revision": 0, "request_id": "e2e-2"}),
        # The same request id again: idempotent, not a second revision.
        tool("t9", "mcp__rca__memory_put", {"id": "e2e-note", "content": "trusted",
                                            "expected_revision": 0, "request_id": "e2e-1"}),
        # A path-shaped memory id: state is addressed logically, never by path.
        tool("t10", "mcp__rca__memory_put", {"id": "../../etc/passwd", "content": "x",
                                             "expected_revision": 0, "request_id": "e2e-3"}),
        # Built-in tools that were removed.
        tool("t11", "Bash", {"command": f"touch {root / 'BASH_MARKER'}"}),
        tool("t12", "Read", {"file_path": str(host_secret)}),
        ([{"type": "text", "text": "done"}], "end_turn"),
    ]


def run(claude, rca, root, base_url_holder, tf, rec):
    remote, host_secret = base_url_holder
    server = tf.serve(rec)
    config = {
        "binary": claude,
        "runtime_home": str(root / "runtime"),
        "state_root": str(root / "state"),
        "remote_root": str(remote.resolve()),
        "exec_program": rca,
        "exec_args": ["serve", "--root", str(remote.resolve())],
        "model": "claude-synthetic",
        "api_base_url": f"http://127.0.0.1:{server.server_address[1]}",
    }
    config_path = root / "config.json"
    config_path.write_text(json.dumps(config, indent=2))

    env = dict(os.environ)
    env["ANTHROPIC_API_KEY"] = "sk-ant-synthetic-not-a-real-key"
    env["RCA_HOST_CANARY"] = "HOST_ENV_CANARY"
    for leak in ("ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"):
        env.pop(leak, None)
    proc = subprocess.run([rca, "claude-native", "--config", str(config_path), "--", "do the thing"],
                          capture_output=True, text=True, timeout=300, env=env)
    server.shutdown()
    return proc


def results_by_tool(rec, tf):
    """Map each tool_use id to the result the harness fed back."""
    out = {}
    for request in rec.requests:
        for message in request.get("messages") or []:
            content = message.get("content")
            if not isinstance(content, list):
                continue
            for block in content:
                if isinstance(block, dict) and block.get("type") == "tool_result":
                    body = block.get("content")
                    if isinstance(body, list):
                        body = " ".join(b.get("text", "") for b in body if isinstance(b, dict))
                    out[block.get("tool_use_id")] = (bool(block.get("is_error")), str(body))
    return out


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--claude", default=shutil.which("claude"))
    ap.add_argument("--rca", default=str(HERE.parent / "bin" / "rca"))
    args = ap.parse_args()
    if not args.claude or not pathlib.Path(args.claude).exists():
        raise SystemExit("no claude binary; pass --claude")
    if not pathlib.Path(args.rca).exists():
        raise SystemExit(f"no rca binary at {args.rca}; run make")

    tf = load_toolface()
    version = subprocess.run([args.claude, "--version"], capture_output=True,
                             text=True, timeout=30).stdout.strip()
    root = pathlib.Path(tempfile.mkdtemp(prefix="rca-claude-native-e2e-"))
    remote, host_secret = build_world(root)
    rec = tf.Recorder(script(root, remote, host_secret))
    print(f"claude: {version}")
    print(f"evidence: {root}\n")

    proc = run(args.claude, str(pathlib.Path(args.rca).resolve()), root, (remote, host_secret), tf, rec)
    c = Checks()

    c.check("session completed", proc.returncode == 0, f"exit {proc.returncode}: {proc.stderr[-400:]}")
    c.check("model was reached", len(rec.requests) > 0)
    if not rec.requests:
        return c.report()

    # --- tool surface -----------------------------------------------------
    advertised = set()
    for request in rec.requests:
        advertised |= set(tf.advertised(request))
    expected = {
        "mcp__rca__workspace_read", "mcp__rca__workspace_write", "mcp__rca__workspace_list",
        "mcp__rca__workspace_search", "mcp__rca__workspace_exec",
        "mcp__rca__memory_list", "mcp__rca__memory_read", "mcp__rca__memory_put",
        "mcp__rca__memory_delete",
        "mcp__rca__skills_list", "mcp__rca__skills_read", "mcp__rca__skills_put",
        "mcp__rca__skills_delete",
    }
    c.check("only rca tools advertised", advertised == expected,
            f"unexpected {sorted(advertised - expected)}, missing {sorted(expected - advertised)}")

    results = results_by_tool(rec, tf)

    def ok(tid):
        return results.get(tid, (True, ""))[0] is False

    def body(tid):
        return results.get(tid, (True, ""))[1]

    # --- remote work actually happens remotely ----------------------------
    c.check("read served from the remote root", ok("t1") and "NEEDLE" in body("t1"), body("t1")[:200])
    c.check("search served from the remote root", ok("t2") and "src/app.go" in body("t2"), body("t2")[:200])
    c.check("write landed on the executor",
            ok("t3") and (remote / "out" / "written.txt").exists() and
            (remote / "out" / "written.txt").read_text() == "FROM_MODEL")
    c.check("exec ran in the remote root", ok("t4") and str(remote.resolve()) in body("t4"), body("t4")[:200])
    c.check("harness token absent from the executor environment",
            "RCA_STATE_TOKEN" not in body("t4") and "sk-ant-synthetic" not in body("t4"))

    # --- the host is unreachable ------------------------------------------
    c.check("absolute host path refused", not ok("t5") and "HOST_ONLY_CANARY" not in body("t5"), body("t5")[:200])
    c.check("traversal out of the root refused", not ok("t6") and "HOST_ONLY_CANARY" not in body("t6"), body("t6")[:200])
    c.check("host secret unmodified", host_secret.read_text() == "HOST_ONLY_CANARY")

    # --- trusted state ----------------------------------------------------
    c.check("memory put committed", ok("t7") and '"revision":1' in body("t7").replace(" ", ""), body("t7")[:200])
    c.check("stale revision refused", not ok("t8"), body("t8")[:200])
    c.check("repeated request id is idempotent",
            ok("t9") and '"revision":1' in body("t9").replace(" ", ""), body("t9")[:200])
    c.check("path-shaped memory id refused", not ok("t10"), body("t10")[:200])

    journal = root / "state" / "journal.jsonl"
    c.check("journal exists", journal.exists())
    if journal.exists():
        entries = [json.loads(l) for l in journal.read_text().splitlines() if l.strip()]
        # A refusal also carries the current object, so presence of `object` is
        # not what distinguishes a commit — the absence of `error` is.
        committed = [e for e in entries if not e.get("result", {}).get("error")]
        refused = [e for e in entries if e.get("result", {}).get("error")]
        c.check("exactly one commit", len(committed) == 1,
                f"{len(committed)} commits in {len(entries)} entries")
        c.check("the refused mutation is recorded with its reason",
                any(e["result"]["error"] == "revision_conflict" for e in refused),
                f"refusals: {[e.get('result', {}).get('error') for e in refused]}")
        # The replayed request id must return the first receipt, not append a
        # second commit: an idempotent retry that writes twice is not one.
        c.check("the replayed request id added no entry", len(entries) == 2,
                f"{len(entries)} entries: {[e['request'].get('request_id') for e in entries]}")
        c.check("no host secret in the journal", "HOST_ONLY_CANARY" not in journal.read_text())

    # --- removed built-ins ------------------------------------------------
    c.check("Bash refused by the harness",
            not ok("t11") and "No such tool available" in body("t11"), body("t11")[:200])
    c.check("Bash had no side effect", not (root / "BASH_MARKER").exists())
    c.check("Read refused by the harness",
            not ok("t12") and "HOST_ONLY_CANARY" not in body("t12"), body("t12")[:200])

    # --- no unmediated fallback state -------------------------------------
    runtime = root / "runtime"
    strays = []
    for base in (runtime / "config", runtime / "user-home" / ".claude"):
        for name in ("memory", "memories", "skills", "plugins", "agents", "hooks"):
            path = base / name
            if path.exists() and any(path.iterdir() if path.is_dir() else [path]):
                strays.append(str(path))
    c.check("no unmediated state root in the runtime home", not strays, str(strays))
    c.check("audit warning not raised", "does not mediate" not in proc.stderr,
            proc.stderr[-300:])

    # Everything the harness wrote must be inside the runtime home.
    outside = [p for p in root.iterdir()
               if p.name not in {"runtime", "state", "remote", "config.json", "HOST_SECRET"}]
    c.check("harness wrote nothing outside its runtime home", not outside, str(outside))

    print()
    failed = c.report()
    print(f"\nevidence retained: {root}")
    return 1 if failed else 0


if __name__ == "__main__":
    raise SystemExit(main())
