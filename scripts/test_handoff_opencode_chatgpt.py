import base64
import importlib.util
import contextlib
import io
import json
import pathlib
import stat
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('handoff', pathlib.Path(__file__).with_name('handoff-opencode-chatgpt.py'))
handoff = importlib.util.module_from_spec(spec)
spec.loader.exec_module(handoff)


def source():
    payload = base64.urlsafe_b64encode(json.dumps({'exp': 9000}).encode()).decode().rstrip('=')
    return {'openai': {'type': 'oauth', 'access': 'test.' + payload + '.signature', 'refresh': 'synthetic-refresh', 'expires': 8000000, 'accountId': 'test-account'}, 'other': {'custom': 'preserve-even-unknown-schema'}}


class HandoffTests(unittest.TestCase):
    def test_existing_session_translates_without_login(self):
        record = handoff.token_record(source(), 1000)
        self.assertEqual(record['expires_at'], 8000)
        self.assertEqual(record['refresh_token'], 'synthetic-refresh')
        self.assertNotIn('id_token', record)

    def test_access_only_or_expiring_source_is_not_transferred(self):
        for change in ({'refresh': ''}, {'expires': 1300000}, {'access': 'not-a-jwt'}):
            document = source()
            document['openai'].update(change)
            with self.subTest(change=list(change)), self.assertRaises(handoff.HandoffError):
                handoff.token_record(document, 1000)

    def test_atomic_retirement_preserves_other_entries(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / 'auth.json'
            path.write_text(json.dumps(source()))
            raw, document = handoff.load_source(path)
            document['openai']['refresh'] = ''
            handoff.replace_source(path, raw, document)
            current = json.loads(path.read_text())
            self.assertEqual(current['other'], source()['other'])
            self.assertEqual(current['openai']['access'], source()['openai']['access'])
            self.assertFalse(current['openai']['refresh'])
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
            self.assertEqual([p.name for p in pathlib.Path(directory).iterdir()], ['auth.json'])

    def test_concurrent_writer_not_overwritten(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / 'auth.json'
            path.write_text(json.dumps(source()))
            raw, document = handoff.load_source(path)
            changed = {**source(), 'new-provider': {'key': 'synthetic'}}
            path.write_text(json.dumps(changed))
            with self.assertRaises(handoff.HandoffError):
                handoff.replace_source(path, raw, document)
            self.assertEqual(json.loads(path.read_text()), changed)

    def test_remote_credentials_only_travel_on_stdin(self):
        args = SimpleNamespace(context='test', namespace='test')
        record = handoff.token_record(source(), 1000)
        with patch.object(handoff.subprocess, 'run', return_value=SimpleNamespace(returncode=0, stdout='{"ok":true}')) as run:
            handoff.remote(args, 'stage', record)
            argv = run.call_args.args[0]
            self.assertNotIn(record['refresh_token'], str(argv))
            self.assertEqual(json.loads(run.call_args.kwargs['input'])['record'], record)

    def test_remote_failure_never_echoes_secret(self):
        args = SimpleNamespace(context='test', namespace='test')
        with patch.object(handoff.subprocess, 'run', return_value=SimpleNamespace(returncode=1, stdout='synthetic-sensitive-response')):
            with self.assertRaises(handoff.HandoffError) as error:
                handoff.remote(args, 'activate')
            self.assertNotIn('synthetic-sensitive-response', str(error.exception))

    def test_retire_only_removes_access_bridge(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / 'auth.json'
            document = source()
            document['openai']['refresh'] = ''
            path.write_text(json.dumps(document))
            with contextlib.redirect_stdout(io.StringIO()):
                result = handoff.main(['--auth-file', str(path), '--apply', '--retire-local-access'])
            self.assertEqual(result, 0)
            self.assertEqual(json.loads(path.read_text()), {'other': document['other']})

    def test_retire_refuses_refresh_owner(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / 'auth.json'
            document = source()
            path.write_text(json.dumps(document))
            with contextlib.redirect_stderr(io.StringIO()):
                result = handoff.main(['--auth-file', str(path), '--apply', '--retire-local-access'])
            self.assertEqual(result, 1)
            self.assertEqual(json.loads(path.read_text()), document)


if __name__ == '__main__':
    unittest.main()
