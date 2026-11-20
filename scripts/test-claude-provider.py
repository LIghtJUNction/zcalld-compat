#!/usr/bin/env python3
"""Real native gateway -> Go worker -> PTY/ConPTY, using only a compiled fake CLI.

Requires the current native core, community library, pluginctl, Claude helper,
and `go test -c ./internal/claudetui` from the same candidate. No actual Claude,
account, token, vendor HTTP, Bun, or source-migration directory is involved.
"""
import argparse
import base64
import concurrent.futures
import ctypes
import hashlib
import http.client
import json
import os
from pathlib import Path
import re
import shutil
import signal
import socket
import struct
import subprocess
import tempfile
import time
import unittest

ARGS = None


class OwnedChild:
    def __init__(self, pid):
        self.pid = int(pid)
        self.handle = None
        if os.name == 'nt':
            self.kernel = ctypes.WinDLL('kernel32', use_last_error=True)
            self.kernel.OpenProcess.argtypes = [ctypes.c_ulong, ctypes.c_int, ctypes.c_ulong]
            self.kernel.OpenProcess.restype = ctypes.c_void_p
            self.kernel.WaitForSingleObject.argtypes = [ctypes.c_void_p, ctypes.c_ulong]
            self.kernel.WaitForSingleObject.restype = ctypes.c_ulong
            self.kernel.CloseHandle.argtypes = [ctypes.c_void_p]
            self.kernel.CloseHandle.restype = ctypes.c_int
            self.handle = self.kernel.OpenProcess(0x00100000, 0, self.pid)
            if not self.handle:
                raise AssertionError('could not retain the owned fake child identity')

    def wait(self, seconds=6):
        if self.handle:
            if self.kernel.WaitForSingleObject(self.handle, int(seconds * 1000)) != 0:
                raise AssertionError('owned fake Claude did not exit')
            return
        end = time.monotonic() + seconds
        while True:
            try:
                os.kill(self.pid, 0)
            except ProcessLookupError:
                return
            if time.monotonic() >= end:
                raise AssertionError('owned fake Claude did not exit')
            time.sleep(.02)

    def close(self):
        if self.handle:
            self.kernel.CloseHandle(self.handle)
            self.handle = None


class NativeClaudeProvider(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='zcalld-provider-native-')
        self.root = Path(self.temporary.name).resolve()
        self.process = None
        self.children = []
        self.addCleanup(self.cleanup)
        self.env = {k: v for k, v in os.environ.items()
                    if not k.startswith(('ANTHROPIC_', 'CLAUDE_', 'MAGPIE_', 'ZCALLD_', 'FAKE_'))
                    and k not in ('BUN_OPTIONS', 'NODE_OPTIONS', 'NOTIFY_SOCKET')
                    and k.upper() not in ('HTTP_PROXY', 'HTTPS_PROXY', 'ALL_PROXY', 'NO_PROXY')}
        self.env['NO_PROXY'] = '127.0.0.1,localhost'
        for variable in ('HOME', 'USERPROFILE', 'XDG_CONFIG_HOME', 'XDG_CACHE_HOME',
                         'XDG_DATA_HOME', 'XDG_STATE_HOME', 'APPDATA', 'LOCALAPPDATA',
                         'TMPDIR', 'TMP', 'TEMP'):
            directory = self.root / variable.lower()
            directory.mkdir(mode=0o700)
            self.env[variable] = str(directory)
        self.empty_path = self.root / 'no-tools-on-path'
        self.empty_path.mkdir(mode=0o700)
        if os.name == 'nt':
            # Match the official archive's DLL set without restoring host tools.
            dll_source = Path(os.environ.get('LIBCURL_DLL_DIR') or ARGS.binary.parent)
            runtime_dlls = list(dll_source.glob('*.dll'))
            self.assertTrue(any('curl' in path.name.lower() for path in runtime_dlls),
                            'Windows fixture needs the official libcurl runtime DLL set')
            for path in runtime_dlls:
                self.assertTrue(path.is_file() and not path.is_symlink(),
                                'Windows fixture runtime DLL must be a regular file')
                shutil.copy2(path, self.empty_path / path.name)
        self.env['PATH'] = str(self.empty_path)  # A true no-Bun path.
        self.env['ZCALLD_PLUGINCTL'] = str(ARGS.pluginctl)
        self.env['ANTHROPIC_API_KEY'] = 'SYNTHETIC-MUST-NOT-REACH-CLI'
        self.env['ANTHROPIC_BASE_URL'] = 'https://invalid.example/never-used'
        self.env['NODE_OPTIONS'] = 'SYNTHETIC-MUST-NOT-REACH-CLI'
        self.config = self.root / 'config.conf'
        self.extensions = self.root / 'extensions'
        self.extensions.mkdir(mode=0o700)
        self.protect(self.root, self.extensions, *(Path(self.env[k]) for k in ('HOME', 'USERPROFILE')))
        self.accounts = []
        for n, ownership in enumerate(('current', 'saved')):
            home = self.root / ('account-' + str(n))
            home.mkdir(mode=0o700)
            directory = home / '.claude' if ownership == 'current' else home / 'saved'
            directory.mkdir(mode=0o700)
            profile = home / '.claude.json' if ownership == 'current' else directory / '.claude.json'
            self.protect(home, directory)
            self.write(profile, {'oauthAccount': {'emailAddress': 'fixture-' + str(n) + '@example.invalid',
                                                 'organizationUuid': 'fixture-organization',
                                                 'accountUuid': 'fixture-member-' + str(n)},
                                 'unrelated': 'preserve-synthetic-profile'})
            self.accounts.append({'config_dir': str(directory), 'profile': str(profile),
                                  'ownership': ownership, 'enabled': True})
            self.fixture(n)
        bindings = self.root / 'bindings.json'
        self.write(bindings, {'version': 1, 'claude_binary': str(ARGS.fake_claude),
                              'interactive_helper': str(ARGS.claude_helper),
                              'trust_workspace': False, 'accounts': self.accounts})
        self.before = {a['profile']: Path(a['profile']).read_bytes() for a in self.accounts}
        # This is the actual Rust passthrough, not pluginctl invoked in isolation.
        plan = self.cli('plugins', 'claude-provider', 'bind', 'plan', '--file', str(bindings))
        self.assertFalse(plan['applied'])
        self.assertEqual(plan['accounts'], 2)
        self.assertEqual(self.launches(), [])
        result = self.cli('plugins', 'claude-provider', 'bind', 'apply', '--file', str(bindings),
                          '--expect-plan', plan['plan_hash'])
        self.assertTrue(result['applied'])
        self.assertFalse(result['credentials_imported'])
        self.assertFalse(result['scheduler_changed'])
        self.assertEqual(self.launches(), [])
        self.state = json.loads((self.extensions / 'maintenance/claude/state.json').read_text())
        self.assertFalse(self.state['policy']['enabled'])
        self.keys = []
        for account in self.accounts:
            key = next(key for key, value in self.state['adapter']['accounts'].items()
                       if value['profile'] == account['profile'])
            self.keys.append(key)
            self.assertTrue(self.state['adapter']['accounts'][key]['no_extra'])
            self.assertFalse(self.state['adapter']['accounts'][key]['automatic'])
        native = self.cli('plugins', 'claude-provider', 'native-config')
        self.assertEqual(set(native), {'claude_only', 'root', 'pluginctl'})
        self.assertTrue(native['claude_only'])
        self.binding = self.root / 'native.json'
        self.write(self.binding, native)
        digest = hashlib.sha256(ARGS.community_plugin.read_bytes()).hexdigest()
        self.cli('plugins', 'install', str(ARGS.community_plugin), '--name', 'claude-native', '--sha256', digest)
        self.cli('plugins', 'config', 'claude-native', '--file', str(self.binding))
        self.cli('plugins', 'enable', 'claude-native')
        self.write_config()
        self.log = (self.root / 'gateway.log').open('w+b')
        self.assertFalse((self.extensions / 'host.json').exists())
        self.assertFalse((self.extensions / 'plugin-auth.json').exists())

    def protect(self, *paths):
        result = subprocess.run([str(ARGS.fake_claude), '--provider-protect-fixture',
                                 *(str(path) for path in paths)], env=self.env,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10)
        self.assertEqual(result.returncode, 0, 'synthetic fixture permission setup failed')

    def write(self, path, value):
        path.write_text(json.dumps(value) + '\n', encoding='utf-8')
        path.chmod(0o600)
        self.protect(path)

    def fixture(self, account=0, **updates):
        path = Path(self.accounts[account]['config_dir']) / 'provider-fixture.json'
        self.write(path, {'magic': 'zcalld-native-provider-fixture-v1', 'mode': 'text', **updates})

    def cli(self, *arguments, success=True):
        result = subprocess.run([str(ARGS.binary), *arguments, '--config', str(self.config), '--json'],
                                env=self.env, cwd=self.root, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, timeout=30)
        if not success:
            self.assertNotEqual(result.returncode, 0)
            return result
        self.assertEqual(result.returncode, 0, result.stderr.decode(errors='replace')[-2048:])
        return json.loads(result.stdout)

    def write_config(self, timeout=45000):
        with socket.socket() as listener:
            listener.bind(('127.0.0.1', 0))
            self.port = listener.getsockname()[1]
        text = (f'[server]\nlisten=127.0.0.1:{self.port}\nunix_socket=none\ntrust_loopback=true\n'
                f'workers=4\nqueue=4\nupstream_timeout_ms={timeout}\n')
        for n, key in enumerate(self.keys):
            text += (f'[provider fixture{n}]\nbase_url=http://127.0.0.1:9/v1\nprotocol=anthropic\n'
                     f'auth=none\nplugin=builtin:claude\nplugin_account={key}\n'
                     f'[route fixture{n}]\nprovider=fixture{n}\nmodel=haiku\n')
        text += ('[provider other]\nbase_url=http://127.0.0.1:9/v1\nprotocol=anthropic\n'
                 'auth=none\nplugin=builtin:codex\n[route other]\nprovider=other\nmodel=haiku\n')
        self.config.write_text(text, encoding='utf-8')
        self.config.chmod(0o600)
        self.protect(self.config)

    def start(self):
        kwargs = {'creationflags': subprocess.CREATE_NEW_PROCESS_GROUP} if os.name == 'nt' else {}
        self.process = subprocess.Popen([str(ARGS.binary), 'serve', '--config', str(self.config)],
                                        env=self.env, cwd=self.root, stdout=self.log, stderr=self.log, **kwargs)
        end = time.monotonic() + 10
        while time.monotonic() < end:
            if self.process.poll() is not None:
                self.log.seek(0)
                self.fail(self.log.read().decode(errors='replace')[-2048:])
            try:
                connection = http.client.HTTPConnection('127.0.0.1', self.port, timeout=.3)
                connection.request('GET', '/healthz')
                response = connection.getresponse()
                response.read()
                connection.close()
                if response.status == 200:
                    return
            except OSError:
                pass
            time.sleep(.02)
        self.fail('the isolated gateway did not start')

    def request(self, body):
        connection = http.client.HTTPConnection('127.0.0.1', self.port, timeout=50)
        try:
            connection.request('POST', '/v1/messages', json.dumps(body),
                               {'Content-Type': 'application/json', 'User-Agent': 'synthetic-provider-fixture'})
            response = connection.getresponse()
            data = response.read((8 << 20) + 1)
            self.assertLessEqual(len(data), 8 << 20)
            return response.status, response.getheader('content-type', ''), data
        finally:
            connection.close()

    def message(self, account=0, **updates):
        return {'model': 'fixture' + str(account), 'max_tokens': 256,
                'messages': [{'role': 'user', 'content': 'hello native fixture'}], **updates}

    def launches(self, account=None, kind='launch'):
        accounts = self.accounts if account is None else [self.accounts[account]]
        return [json.loads(path.read_text()) for item in accounts
                for path in sorted(Path(item['config_dir']).glob(kind + '-*.json'))]

    def wait_record(self, account=0, kind='prompt', seconds=10):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            records = self.launches(account, kind)
            if records:
                return records[-1]
            time.sleep(.01)
        self.fail('the fake CLI did not reach ' + kind)

    def wait_markers_gone(self, account=0, seconds=7):
        directory = Path(self.accounts[account]['config_dir'])
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            if not list(directory.parent.glob(directory.name + '.run-*.lock')):
                return
            time.sleep(.02)
        self.fail('native account lease remained after confirmed fixture shutdown')

    def raw_request(self, body):
        stream = socket.create_connection(('127.0.0.1', self.port), timeout=5)
        data = json.dumps(body).encode()
        stream.sendall((f'POST /v1/messages HTTP/1.1\r\nHost: 127.0.0.1:{self.port}\r\n'
                        'User-Agent: synthetic-provider-fixture\r\nContent-Type: application/json\r\n'
                        f'Content-Length: {len(data)}\r\n\r\n').encode() + data)
        return stream

    def reset_socket(self, stream):
        # FIN is a legal HTTP half-close. RST, as in the existing core abort
        # fixture, is the actual disconnected-client cancellation contract.
        stream.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER,
                          struct.pack('hh' if os.name == 'nt' else 'ii', 1, 0))
        stream.close()

    def cleanup(self):
        if self.process and self.process.poll() is None:
            self.process.send_signal(signal.CTRL_BREAK_EVENT if os.name == 'nt' else signal.SIGTERM)
            try:
                self.process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=5)
                self.fail('owned gateway did not stop within the fixture drain budget')
        for child in self.children:
            child.close()
        if hasattr(self, 'log'):
            self.log.close()
        self.temporary.cleanup()

    def test_standalone_binding_and_json_sse_preserve_both_native_profile_layouts(self):
        self.start()
        for account, stream in ((0, False), (1, True)):
            with self.subTest(account=account, stream=stream):
                status, kind, data = self.request(self.message(account, stream=stream))
                self.assertEqual(status, 200, data)
                if stream:
                    self.assertIn('text/event-stream', kind)
                    self.assertEqual(data.count(b'event: message_stop\n'), 1)
                    self.assertIn(b'hello native fixture', data)
                else:
                    result = json.loads(data)
                    self.assertEqual(result['stop_reason'], 'end_turn')
                    self.assertIn('hello native fixture', result['content'][0]['text'])
                self.wait_markers_gone(account)
                launch = [x for x in self.launches(account) if '-p' not in x['args']]
                self.assertEqual(len(launch), 1)
                self.assertFalse(launch[0]['secret_env_present'])
                self.assertEqual(launch[0]['max_tokens'], '256')
                if account == 0:
                    self.assertEqual(launch[0]['config_env'], '')
                    self.assertEqual(Path(launch[0]['home']), Path(self.accounts[0]['profile']).parent)
                else:
                    self.assertEqual(launch[0]['config_env'], self.accounts[1]['config_dir'])
                usage = [x for x in self.launches(account) if '-p' in x['args']]
                self.assertEqual(len(usage), 1)
                self.assertEqual(usage[0]['args'][:2], ['-p', '/usage'])
        for path, before in self.before.items():
            self.assertEqual(Path(path).read_bytes(), before)
        self.assertFalse((self.extensions / 'plugin-auth.json').exists())

    def test_inline_image_reaches_the_actual_terminal_paste(self):
        self.start()
        content = [{'type': 'text', 'text': 'inspect fixture'},
                   {'type': 'image', 'source': {'type': 'base64', 'media_type': 'image/png',
                                               'data': base64.b64encode(b'IMAGE-FIXTURE').decode()}}]
        status, _, data = self.request(self.message(messages=[{'role': 'user', 'content': content}]))
        self.assertEqual(status, 200, data)
        self.assertIn('IMAGE-FIXTURE', self.wait_record()['text'])

    def test_unsupported_request_is_rejected_before_native_process_or_usage(self):
        self.start()
        for update in ({'temperature': .1}, {'stop_sequences': ['stop']},
                       {'thinking': {'type': 'enabled', 'budget_tokens': 1000}},
                       {'messages': [{'role': 'user', 'content': [{'type': 'image', 'source':
                            {'type': 'url', 'url': 'https://invalid.example/never-fetch'}}]}]}):
            with self.subTest(update=update):
                status, _, _ = self.request(self.message(**update))
                self.assertEqual(status, 400)
        self.assertEqual(self.launches(), [])

    def test_full_account_cap_prevents_interactive_generation(self):
        self.fixture(used=100)
        self.start()
        status, _, data = self.request(self.message())
        self.assertEqual(status, 429, data)
        self.assertEqual(len(self.launches()), 1)
        self.assertEqual(self.launches()[0]['args'][:2], ['-p', '/usage'])
        self.assertEqual(self.launches(kind='prompt'), [])
        self.wait_markers_gone()

    def tools(self, account=0, text='parallel tools'):
        return self.message(account, messages=[{'role': 'user', 'content': text}], tools=[
            {'name': 'search/one', 'input_schema': {'type': 'object'}},
            {'name': 'search/two', 'input_schema': {'type': 'object'}}])

    def continuation(self, original, response):
        answer = {'role': 'assistant', 'content': response['content']}
        results = [{'type': 'tool_result', 'tool_use_id': item['id'], 'content': 'done-' + str(n)}
                   for n, item in enumerate(response['content']) if item['type'] == 'tool_use']
        return original | {'messages': original['messages'] + [answer, {'role': 'user', 'content': results}]}

    def test_parallel_tools_bind_complete_context_account_and_consume_once(self):
        self.fixture(mode='tools')
        self.start()
        original = self.tools()
        status, _, data = self.request(original)
        self.assertEqual(status, 200, data)
        response = json.loads(data)
        self.assertEqual(response['stop_reason'], 'tool_use')
        self.assertEqual(len(response['content']), 2)
        for block in response['content']:
            self.assertRegex(block['id'], r'^toolu_zc_[0-9a-f]{64}$')
        self.assertNotEqual(response['content'][0]['id'], response['content'][1]['id'])
        continuation = self.continuation(original, response)
        for wrong in (continuation | {'model': 'fixture1'},
                      continuation | {'messages': [{'role': 'user', 'content': 'different history'}] + continuation['messages'][1:]}):
            self.assertEqual(self.request(wrong)[0], 409)
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            results = list(pool.map(self.request, [continuation, continuation]))
        self.assertEqual(sorted(item[0] for item in results), [200, 409])
        good = next(json.loads(item[2]) for item in results if item[0] == 200)
        self.assertEqual(good['content'][0]['text'], 'tools-complete')
        self.assertEqual(self.request(continuation)[0], 409)
        self.assertEqual(len([x for x in self.launches() if '-p' not in x['args']]), 1)
        self.assertEqual(len(self.launches(kind='tool-results')), 1)
        self.wait_markers_gone()

    def test_concurrent_account_tool_sessions_do_not_cross(self):
        for n in range(2):
            self.fixture(n, mode='tools')
        self.start()
        originals = [self.tools(n, 'context-' + str(n)) for n in range(2)]
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            first = list(pool.map(self.request, originals))
        self.assertEqual([x[0] for x in first], [200, 200])
        continuations = [self.continuation(originals[n], json.loads(first[n][2])) for n in range(2)]
        crossed = dict(continuations[0])
        crossed['messages'] = continuations[0]['messages'][:-1] + [continuations[1]['messages'][-1]]
        self.assertEqual(self.request(crossed)[0], 409)
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            final = list(pool.map(self.request, continuations))
        self.assertEqual([x[0] for x in final], [200, 200])

    def test_identity_change_before_ready_never_sends_the_prompt(self):
        self.fixture(switch_identity=True)
        self.start()
        status, _, _ = self.request(self.message())
        self.assertEqual(status, 502)
        self.assertEqual(self.launches(kind='prompt'), [])
        self.wait_markers_gone()

    def test_client_reset_confirms_ignored_term_pty_child_exit_before_lease_release(self):
        self.fixture(mode='hold')
        self.start()
        stream = self.raw_request(self.message())
        self.addCleanup(stream.close)
        record = self.wait_record()
        child = OwnedChild(record['pid'])
        self.children.append(child)
        directory = Path(self.accounts[0]['config_dir'])
        self.assertTrue(list(directory.parent.glob(directory.name + '.run-*.lock')))
        self.reset_socket(stream)
        child.wait()
        self.wait_markers_gone()

    def test_abort_during_preflight_usage_drains_before_releasing_account(self):
        self.fixture(usage_hold=True)
        self.start()
        stream = self.raw_request(self.message())
        self.addCleanup(stream.close)
        record = self.wait_record(kind='usage')
        child = OwnedChild(record['pid'])
        self.children.append(child)
        self.reset_socket(stream)
        child.wait()
        self.wait_markers_gone()
        self.assertEqual(self.launches(kind='prompt'), [])

    def test_ordinary_community_config_and_other_provider_stay_rejected(self):
        before = (self.extensions / 'registry.json').read_bytes()
        self.write(self.extensions / 'host.json', {'bun': str(self.root / 'missing-bun')})
        self.cli('plugins', 'claude-provider', 'native-config', success=False)
        self.assertEqual((self.extensions / 'registry.json').read_bytes(), before)
        (self.extensions / 'host.json').unlink()  # Only the fixture's own marker.
        ordinary = self.root / 'ordinary.json'
        self.write(ordinary, {'root': str(self.extensions), 'pluginctl': str(ARGS.pluginctl)})
        rejected = self.root / 'ordinary.conf'
        rejected.write_text(self.config.read_text() +
                            f'\n[plugin ordinary]\npath={ARGS.community_plugin}\nconfig_file={ordinary}\n')
        result = subprocess.run([str(ARGS.binary), 'check', '--config', str(rejected), '--json'],
                                env=self.env, cwd=self.root, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, timeout=10)
        self.assertNotEqual(result.returncode, 0)
        self.start()
        status, _, _ = self.request(self.message(model='other'))
        self.assertGreaterEqual(status, 400)
        self.assertEqual(self.launches(), [])


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('binary', 'community-plugin', 'pluginctl', 'claude-helper', 'fake-claude'):
        parser.add_argument('--' + name, type=Path, required=True)
    ARGS, remaining = parser.parse_known_args()
    for name, path in vars(ARGS).items():
        setattr(ARGS, name, path.resolve(strict=True))
    unittest.main(argv=[__file__, *remaining])
