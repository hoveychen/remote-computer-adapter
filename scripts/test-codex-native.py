#!/usr/bin/env python3
"""Real Codex + scripted Responses API + mountless/networkless Linux executor.
No model account, API key or vendor registry is required. Evidence is retained
outside worktrees. Requires Docker and an image with /usr/local/bin/codex.
"""
import argparse
import base64
import http.server
import json
import os
import pathlib
import selectors
import shlex
import shutil
import signal
import subprocess
import tempfile
import threading
import time
import uuid


def run(argv, **kw):
    try:
        return subprocess.run(argv, check=True, capture_output=True, text=True, timeout=30, **kw).stdout.strip()
    except subprocess.CalledProcessError as exc:
        raise RuntimeError(f'{argv[0]} exited {exc.returncode}: {exc.stderr.strip()}') from exc


def finish(proc, timeout=45):
    try:
        return proc.communicate(timeout=timeout)
    except subprocess.TimeoutExpired:
        os.killpg(proc.pid, signal.SIGTERM)
        try:
            proc.communicate(timeout=5)
        except subprocess.TimeoutExpired:
            os.killpg(proc.pid, signal.SIGKILL)
            proc.communicate(timeout=5)
        raise


class RPC:
    def __init__(self, argv, cwd, env, log):
        self.log = open(log, 'w')
        self.proc = subprocess.Popen(argv, cwd=cwd, env=env, stdin=subprocess.PIPE,
                                     stdout=subprocess.PIPE, stderr=self.log, start_new_session=True)
        self.sel = selectors.DefaultSelector()
        self.sel.register(self.proc.stdout, selectors.EVENT_READ)
        self.buf = b''
        self.history = []
        self.seq = 0

    def send(self, method, params=None, request_id=None):
        q = {'jsonrpc': '2.0', 'method': method}
        if params is not None:
            q['params'] = params
        if request_id is not None:
            q['id'] = request_id
        self.proc.stdin.write((json.dumps(q) + '\n').encode())
        self.proc.stdin.flush()

    def call(self, method, params):
        self.seq += 1
        self.send(method, params, self.seq)
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            if b'\n' in self.buf:
                line, self.buf = self.buf.split(b'\n', 1)
                if not line:
                    continue
                r = json.loads(line)
                self.history.append(r)
                if r.get('id') == self.seq:
                    return r
            elif self.sel.select(.1):
                data = os.read(self.proc.stdout.fileno(), 1024 * 1024)
                if not data:
                    raise RuntimeError('RPC EOF: ' + method)
                self.buf += data
        raise TimeoutError(method)

    def close(self):
        self.proc.stdin.close()
        try:
            self.proc.wait(timeout=3)
        except subprocess.TimeoutExpired:
            os.killpg(self.proc.pid, signal.SIGKILL)
            self.proc.wait(timeout=3)
        self.sel.close()
        self.log.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--rca', required=True)
    parser.add_argument('--codex', default=shutil.which('codex'))
    parser.add_argument('--image', default='claw-runtime:latest')
    args = parser.parse_args()
    rca = str(pathlib.Path(args.rca).resolve())
    codex = str(pathlib.Path(args.codex).resolve())
    docker = str(pathlib.Path(shutil.which('docker')).resolve())
    docker_host = run([docker, 'context', 'inspect', '--format', '{{.Endpoints.docker.Host}}'])
    dc = [docker, '--host', docker_host]
    p = pathlib.Path(tempfile.mkdtemp(prefix='rca-native-e2e-')).resolve()
    print('evidence:', p, flush=True)
    name = 'rca-native-test-' + uuid.uuid4().hex[:12]
    result = {'harness_version': run([codex, '--version']), 'checks': {}}
    requests = []
    server = None
    container_created = False

    def check(name, value):
        result['checks'][name] = bool(value)
        assert value, name
        print('PASS', name, flush=True)

    try:
        container_created = True
        run(dc + ['run', '-d', '--name', name, '--network', 'none', '--entrypoint', '/bin/sh',
                  args.image, '-c', 'mkdir -p /workspace /tmp/executor-home; exec sleep 600'])
        result['executor_version'] = run(dc + ['exec', name, '/usr/local/bin/codex', '--version'])
        config = {'binary': codex, 'runtime_home': str(p / 'harness'), 'state_root': str(p / 'state'),
                  'remote_cwd': '/workspace', 'exec_program': docker,
                  'exec_args': ['--host', docker_host, 'exec', '-i', '-w', '/workspace', name,
                                '/usr/bin/env', '-i', 'PATH=/usr/local/bin:/usr/bin:/bin',
                                'HOME=/tmp/executor-home', 'CODEX_HOME=/tmp/executor-home',
                                '/usr/local/bin/codex', 'exec-server', '--listen', 'stdio'],
                  'model': 'gpt-5.6-sol'}
        sentinel = p / 'TRUSTED_ONLY'
        sentinel.write_text('TRUSTED_UNCHANGED')
        # These paths exist only on the trusted host, never in the container.
        journal = p / 'state' / 'journal.jsonl'
        shell = 'uname -s; pwd; printf REMOTE_EXEC > /workspace/exec-marker; '
        shell += 'printf "\\nENV_START\\n"; env; printf "ENV_END\\n"; '
        shell += 'if cat ' + shlex.quote(str(sentinel)) + '; then echo HOST_READ_BREACH; else echo HOST_READ_DENIED; fi'
        patch = '*** Begin Patch\n*** Add File: /workspace/patch-marker\n+REMOTE_PATCH\n*** End Patch'
        host_patch = '*** Begin Patch\n*** Update File: ' + str(sentinel) + '\n@@\n-TRUSTED_UNCHANGED\n+BREACH\n*** End Patch'
        journal_patch = '*** Begin Patch\n*** Update File: ' + str(journal) + '\n@@\n-BREACH\n+BREACH2\n*** End Patch'
        code = [
            'text(await tools.exec_command(' + json.dumps({'cmd': shell, 'login': False, 'yield_time_ms': 1000}) + '));',
            'text(await tools.apply_patch(' + json.dumps(patch) + '));',
            'text(await tools.mcp__rca_state__memory_put({id:"note",content:"TRUSTED_MEMORY",expected_revision:0,request_id:"memory-1"}));',
            'text(await tools.mcp__rca_state__skills_put({id:"skill",content:"TRUSTED_SKILL",expected_revision:0,request_id:"skill-1"}));',
            'text(await tools.apply_patch(' + json.dumps(host_patch) + '));',
            'text(await tools.apply_patch(' + json.dumps(journal_patch) + '));',
            'text(await tools.exec_command(' + json.dumps({'cmd': 'printf BREACH >> ' + shlex.quote(str(journal)), 'login': False}) + '));',
            'text(await tools.mcp__rca_state__memory_put({id:"note",content:"TRUSTED_MEMORY",expected_revision:0,request_id:"memory-1"}));',
            'text(await tools.mcp__rca_state__memory_put({id:"note",content:"WRONG_REVISION",expected_revision:0,request_id:"conflict-1"}));',
            'text(await tools.mcp__rca_state__skills_put({id:"../escape",content:"PATH_ATTACK",expected_revision:0,request_id:"path-1"}));',
            'text(await tools.mcp__rca_state__memory_read({id:"note"})); text(await tools.mcp__rca_state__skills_read({id:"skill"}));',
        ]

        native_filename = '2026-09-06T01-02-03-native-e2e.md'
        native_path = 'extensions/ad_hoc/notes/' + native_filename
        code.extend([
            'text(await tools.memories__add_ad_hoc_note(' + json.dumps({'filename': native_filename, 'note': 'NATIVE_CANONICAL_NOTE'}) + '));',
            'text(await tools.memories__read(' + json.dumps({'path': native_path}) + '));',
            'text(await tools.memories__search({queries:["NATIVE_CANONICAL_NOTE"]}));',
            'text(await tools.memories__list({path:"extensions/ad_hoc/notes"}));',
            'text(await tools.skills__list({authority:{kind:"host"}}));',
            'text(await tools.skills__read({authority:{kind:"host"},package:"imagegen",resource:"native-skill:host:imagegen:1:SKILL.md"}));',
        ])

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                requests.append(body)
                n = len(requests)
                rid = 'resp_' + str(n)
                if n <= len(code):
                    item = {'type': 'custom_tool_call', 'call_id': 'call_' + str(n),
                            'name': 'exec', 'input': code[n - 1]}
                else:
                    item = {'type': 'message', 'id': 'msg_done', 'role': 'assistant',
                            'content': [{'type': 'output_text', 'text': 'E2E_DONE'}]}
                events = [{'type': 'response.created', 'response': {'id': rid}},
                          {'type': 'response.output_item.done', 'item': item},
                          {'type': 'response.completed', 'response': {'id': rid,
                           'usage': {'input_tokens': 1, 'output_tokens': 1, 'total_tokens': 2}}}]
                self.send_response(200)
                self.send_header('Content-Type', 'text/event-stream')
                self.end_headers()
                for e in events:
                    self.wfile.write(('data: ' + json.dumps(e) + '\n\n').encode())

        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        config['provider_url'] = 'http://127.0.0.1:' + str(server.server_port) + '/v1'
        (p / 'config.json').write_text(json.dumps(config, indent=2))
        env = dict(os.environ, SECRET_CANARY='SYNTHETIC_SECRET_NEVER_REMOTE', OPENAI_API_KEY='SYNTHETIC_PROVIDER_KEY')
        proc = subprocess.Popen([rca, 'codex-native', '--config', str(p / 'config.json'), '--',
                                 'exec', '--json', '--', 'Run the scripted native boundary acceptance probe.'],
                                stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                text=True, env=env, start_new_session=True)
        out, err = finish(proc, 60)
        (p / 'stdout.jsonl').write_text(out)
        (p / 'stderr.txt').write_text(err)
        (p / 'requests.json').write_text(json.dumps(requests, indent=2))
        check('codex_completed', proc.returncode == 0 and 'E2E_DONE' in out and len(requests) == len(code) + 1)
        advertised_custom_tools = [tool['name'] for item in requests[0].get('input', [])
                                   if item.get('type') == 'additional_tools'
                                   for tool in item.get('tools', []) if tool.get('type') == 'custom']
        check('custom_tool_name_matches_advertisement', advertised_custom_tools == ['exec'])
        # Assert actual tool outputs, not the scripted assistant's final claim.
        outputs = {}
        for req in requests:
            for item in req.get('input', []):
                if item.get('type') in ('custom_tool_call_output', 'function_call_output'):
                    outputs[item.get('call_id')] = item.get('output')
        (p / 'tool-outputs.json').write_text(json.dumps(outputs, indent=2))
        def output(n):
            return json.dumps(outputs.get('call_' + str(n), ''))
        check('exec_is_linux_remote', 'Linux' in output(1) and '/workspace' in output(1)
              and run(dc + ['exec', name, 'cat', '/workspace/exec-marker']) == 'REMOTE_EXEC')
        check('apply_patch_is_remote', run(dc + ['exec', name, 'cat', '/workspace/patch-marker']) == 'REMOTE_PATCH')
        check('host_file_read_denied', 'HOST_READ_DENIED' in output(1) and 'HOST_READ_BREACH' not in output(1))
        check('host_file_patch_denied', sentinel.read_text() == 'TRUSTED_UNCHANGED' and 'Failed' in output(5))
        check('harness_env_not_forwarded', 'SYNTHETIC_SECRET_NEVER_REMOTE' not in output(1)
              and 'SYNTHETIC_PROVIDER_KEY' not in output(1) and 'RCA_STATE_TOKEN=' not in output(1) and 'RCA_NATIVE_MEMORY_TOKEN=' not in output(1))
        all_entries = [json.loads(line) for line in journal.read_text().splitlines()]
        entries = [r for r in all_entries if r.get('format_version', 1) != 2]
        native_entries = [r for r in all_entries if r.get('format_version') == 2]
        notes = [r for r in native_entries if r['request']['operation'] == 'memory.note.create']
        bundled = [r for r in native_entries if r['request']['operation'] == 'skills.bundled.ensure']
        check('native_note_has_single_go_receipt', len(notes) == 1
              and base64.b64decode(notes[0]['request']['changes'][0]['content_base64']) == b'NATIVE_CANONICAL_NOTE')
        check('native_read_search_list_use_go_state', 'NATIVE_CANONICAL_NOTE' in output(13)
              and 'NATIVE_CANONICAL_NOTE' in output(14) and native_filename in output(15))
        check('native_skills_list_and_read_use_go_state', 'native-skill:host:imagegen:1:SKILL.md' in output(16)
              and 'Image Generation Skill' in output(17))
        check('native_bundled_skills_have_single_go_receipt', len(bundled) == 1
              and bundled[0]['result']['status'] == 'committed'
              and any(change.get('domain') == 'skills.package'
                      and change.get('key') == 'imagegen'
                      and change.get('package', {}).get('files')
                      for change in bundled[0]['request']['changes']))
        check('native_memory_has_no_local_note', not (p / 'harness/memories').exists())
        check('native_skills_have_no_local_cache', not (p / 'harness/skills').exists())
        check('semantic_state_audit', len(entries) == 3 and entries[0]['collection'] == 'memory'
              and entries[0]['result']['object']['content'] == 'TRUSTED_MEMORY'
              and entries[1]['collection'] == 'skills' and entries[1]['result']['object']['content'] == 'TRUSTED_SKILL')
        check('idempotent_mutation', sum(r['request']['request_id'] == 'memory-1' for r in entries) == 1)
        check('revision_conflict_audited', entries[2]['result']['error'] == 'revision_conflict')
        check('path_traversal_denied', 'invalid logical id' in output(10) and not (p / 'escape').exists())
        check('journal_shell_and_patch_denied', 'Failed' in output(6) and 'No such file' in output(7)
              and all('BREACH' not in json.dumps(r) for r in entries))
        check('semantic_reads', 'TRUSTED_MEMORY' in output(11) and 'TRUSTED_SKILL' in output(11))
        # App-server independently exercises explicit environment selection.
        rpc_env = {'PATH': os.environ['PATH'], 'HOME': str(p / 'harness/user-home'), 'CODEX_HOME': str(p / 'harness')}
        rpc = RPC([codex, 'app-server'], str(p), rpc_env, p / 'app-server-stderr.txt')
        try:
            r = rpc.call('initialize', {'clientInfo': {'name': 'rca_acceptance', 'version': '1'},
                                        'capabilities': {'experimentalApi': True}})
            check('app_server_initialized', 'result' in r)
            rpc.send('initialized')
            r = rpc.call('environment/info', {'environmentId': 'third-party'})
            check('registered_remote_cwd', r.get('result', {}).get('cwd') == 'file:///workspace')
            r = rpc.call('config/read', {'includeLayers': False})
            effective = r.get('result', {}).get('config', {})
            check('native_memory_read_enabled_with_background_disabled',
                  effective.get('memories', {}).get('generate_memories') is False
                  and effective.get('memories', {}).get('use_memories') is True
                  and effective.get('features', {}).get('memories') is True
                  and bool(effective.get('memories', {}).get('native_service', {}).get('store_id')))
            check('no_native_memory_consolidation_files', not (p / 'harness/memories').exists())
            for eid in ['local', 'unknown']:
                r = rpc.call('environment/info', {'environmentId': eid})
                check(eid + '_environment_rejected', 'unknown environment id' in r.get('error', {}).get('message', ''))
            run(dc + ['stop', '-t', '1', name])
            r = rpc.call('environment/info', {'environmentId': 'third-party'})
            # environment/info may cache metadata. FS requests must reach the
            # stopped backend, so a separate startup below proves fail closed.
        finally:
            (p / 'app-server-rpc.json').write_text(json.dumps(rpc.history, indent=2))
            rpc.close()
        requests.clear()
        proc = subprocess.Popen([rca, 'codex-native', '--config', str(p / 'config.json'), '--',
                                 'exec', '--json', '--', 'This must fail with the executor stopped.'],
                                stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                text=True, env=env, start_new_session=True)
        out, err = finish(proc, 35)
        (p / 'offline-stdout.jsonl').write_text(out)
        (p / 'offline-stderr.txt').write_text(err)
        check('offline_fails_without_model_or_local_fallback', proc.returncode != 0 and not requests)
        check('trusted_sentinel_unchanged', sentinel.read_text() == 'TRUSTED_UNCHANGED')
        # A malformed stdio backend must also fail before contacting the model.
        bad = dict(config, exec_program='/bin/echo', exec_args=['not-json-rpc'])
        (p / 'bad-config.json').write_text(json.dumps(bad))
        proc = subprocess.Popen([rca, 'codex-native', '--config', str(p / 'bad-config.json'), '--',
                                 'exec', '--json', '--', 'This must fail on protocol error.'],
                                stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                text=True, env=env, start_new_session=True)
        out, err = finish(proc, 35)
        (p / 'protocol-stdout.jsonl').write_text(out)
        (p / 'protocol-stderr.txt').write_text(err)
        check('protocol_error_fails_without_local_fallback', proc.returncode != 0 and not requests)
    except Exception as exc:
        result['failure'] = str(exc)
        raise
    finally:
        (p / 'results.json').write_text(json.dumps(result, indent=2))
        if server:
            server.shutdown()
        if container_created:
            subprocess.run(dc + ['rm', '-f', name], capture_output=True, timeout=20)
        print('retained evidence:', p, flush=True)


if __name__ == '__main__':
    main()
