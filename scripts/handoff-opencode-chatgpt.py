#!/usr/bin/env python3
"""One-time transfer of OAuth refresh ownership to LiteLLM's persistent volume.

No credentials are printed or passed in argv. The current OpenCode access token
can remain as a short-lived bridge for the active session; its refresh token is
removed. New sessions must use the configured LiteLLM provider.
"""
import argparse
import base64
import json
import math
import os
import pathlib
import subprocess
import sys
import tempfile
import time


class HandoffError(Exception):
    pass


def load_source(path):
    try:
        raw = path.read_bytes()
        document = json.loads(raw)
    except (OSError, ValueError):
        raise HandoffError('Cannot read source auth; no credential data was printed.') from None
    if not isinstance(document, dict):
        raise HandoffError('Source auth is not an object.')
    return raw, document


def token_record(document, now):
    source = document.get('openai')
    if not isinstance(source, dict) or source.get('type') != 'oauth':
        raise HandoffError('Source has no OpenAI OAuth session.')
    if not all(isinstance(source.get(key), str) and source[key] for key in ('access', 'refresh')):
        raise HandoffError('Source must contain both access and refresh credentials; it may already be handed off.')
    try:
        part = source['access'].split('.')[1]
        claims = json.loads(base64.urlsafe_b64decode(part + '=' * (-len(part) % 4)))
        expiry = min(claims['exp'], source['expires'] / 1000)
        if not math.isfinite(expiry) or expiry <= now + 600:
            raise ValueError()
        account = source.get('accountId') or claims['https://api.openai.com/auth']['chatgpt_account_id']
        if not isinstance(account, str) or not account:
            raise ValueError()
    except (ValueError, TypeError, KeyError, IndexError):
        raise HandoffError('Source session needs at least ten minutes of valid access and an account ID.') from None
    return {'access_token': source['access'], 'refresh_token': source['refresh'], 'expires_at': int(expiry), 'account_id': account}


def replace_source(path, expected, document):
    # OpenCode has no cross-process writer lock. Refuse a detected concurrent
    # change, preserve unrelated entries, and never create a credential backup.
    if path.read_bytes() != expected:
        raise HandoffError('OpenCode auth changed concurrently; stop other auth writers and retry.')
    fd, temporary = tempfile.mkstemp(prefix='.auth-handoff-', dir=path.parent)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, 'w') as output:
            json.dump(document, output, indent=2)
            output.write('\n')
            output.flush()
            os.fsync(output.fileno())
        if path.read_bytes() != expected:
            raise HandoffError('OpenCode auth changed during preparation; no source replacement made.')
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


# Executed only inside the existing gateway container. stdout is captured and
# parsed by this script, never forwarded. stderr from libraries is suppressed.
REMOTE = r'''
import contextlib, io, json, logging, os, pathlib, sys, tempfile, time
request = json.load(sys.stdin)
path = pathlib.Path(os.environ['CHATGPT_TOKEN_DIR']) / os.environ.get('CHATGPT_AUTH_FILE', 'auth.json')
assert str(path.parent) == '/var/lib/litellm/chatgpt', 'Gateway is not using the persistent auth directory'
staged = path.with_name('.handoff.json')
def write(target, record):
    fd, name = tempfile.mkstemp(prefix='.auth-', dir=target.parent)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, 'w') as out:
            json.dump(record, out)
            out.flush()
            os.fsync(out.fileno())
        os.replace(name, target)
    finally:
        if os.path.exists(name): os.unlink(name)
logging.disable(logging.CRITICAL)
try:
    with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
        existing = json.loads(path.read_text())
        action = request['action']
        if action == 'stage':
            if existing.get('refresh_token'):
                raise RuntimeError('Destination already owns refresh; do not overwrite rotated credentials')
            write(staged, request['record'])
            result = {'staged': True}
        elif action == 'discard':
            staged.unlink(missing_ok=True)
            result = {'discarded': True}
        elif action == 'activate':
            if existing.get('refresh_token'):
                raise RuntimeError('Destination already owns refresh')
            os.replace(staged, path)
            result = {'activated': True}
        elif action == 'verify-refresh':
            from litellm.llms.chatgpt.authenticator import Authenticator
            if not existing.get('refresh_token'):
                raise RuntimeError('Destination does not own refresh')
            authenticator = Authenticator()
            # Exercise native get_access_token's expired-token branch without
            # expiring the live worker's file or permitting device login.
            authenticator._is_token_expired = lambda *args: True
            def no_login(*args, **kwargs):
                raise RuntimeError('Interactive login disabled during verification')
            authenticator._login_device_code = no_login
            access = authenticator.get_access_token()
            persisted = json.loads(path.read_text())
            if persisted.get('access_token') != access or not persisted.get('refresh_token') or persisted.get('expires_at', 0) <= time.time() + 300:
                raise RuntimeError('Refreshed session was not persisted')
            # Access-only bridge for this still-running native OpenCode session.
            # No refresh credential ever returns to the workstation.
            result = {'refresh_verified': True, 'record': {k: persisted[k] for k in ('access_token', 'account_id', 'expires_at')}}
        else:
            raise RuntimeError('Unknown handoff action')
    print(json.dumps({'ok': True, **result}))
except Exception as error:
    # Native OAuth exceptions can contain credential-bearing response objects.
    print(json.dumps({'ok': False, 'error_type': type(error).__name__}))
    sys.exit(1)
'''


def remote(args, action, record=None):
    request = {'action': action}
    if record is not None:
        request['record'] = record
    result = subprocess.run(['kubectl', '--context', args.context, '--namespace', args.namespace,
                             'exec', '-i', 'deployment/litellm', '-c', 'litellm', '--', 'python', '-c', REMOTE],
                            input=json.dumps(request), capture_output=True, text=True, timeout=120)
    try:
        output = json.loads(result.stdout)
    except ValueError:
        raise HandoffError('Gateway handoff command failed; output suppressed to protect credentials.') from None
    if result.returncode or not output.get('ok'):
        raise HandoffError('Gateway operation failed; existing credentials were not printed. Check handoff state before retrying.')
    return output


def require_no_sync():
    timer = subprocess.run(['systemctl', '--user', 'is-active', 'ai-flow-chatgpt-sync.timer'], capture_output=True, text=True, timeout=10)
    service = subprocess.run(['systemctl', '--user', 'is-active', 'ai-flow-chatgpt-sync.service'], capture_output=True, text=True, timeout=10)
    if timer.returncode == 0 or service.returncode == 0:
        raise HandoffError('Stop the old access-token sync timer/service before handing off ownership.')


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    root = pathlib.Path(os.getenv('XDG_DATA_HOME', str(pathlib.Path.home() / '.local/share')))
    parser.add_argument('--auth-file', type=pathlib.Path, default=root / 'opencode/auth.json')
    parser.add_argument('--context', default='default')
    parser.add_argument('--namespace', default='llm-gateway')
    parser.add_argument('--apply', action='store_true')
    action = parser.add_mutually_exclusive_group()
    action.add_argument('--verify-refresh', action='store_true', help='After handoff, test one real native refresh and preserve the active session access bridge.')
    action.add_argument('--retire-local-access', action='store_true', help='Only after restarting OpenCode on the gateway: remove the leftover access-only native entry.')
    args = parser.parse_args(argv)
    try:
        if os.getenv('OPENCODE_AUTH_CONTENT'):
            raise HandoffError('OPENCODE_AUTH_CONTENT overrides the file; resolve that source before migration.')
        raw, document = load_source(args.auth_file)
        if args.retire_local_access:
            if not args.apply:
                raise HandoffError('--retire-local-access requires --apply after restarting OpenCode.')
            source = document.get('openai')
            if source is None:
                print('No native OpenAI auth entry remains.')
                return 0
            if not isinstance(source, dict) or source.get('type') != 'oauth' or source.get('refresh'):
                raise HandoffError('Refusing to remove an entry that is not the retired access-only bridge.')
            del document['openai']
            replace_source(args.auth_file, raw, document)
            print('Removed the retired native access-only entry. Gateway credentials were not touched.')
            return 0
        if args.verify_refresh:
            if not args.apply:
                raise HandoffError('--verify-refresh requires --apply.')
            require_no_sync()
            if document.get('openai', {}).get('refresh'):
                raise HandoffError('OpenCode still owns refresh; complete handoff first.')
            fresh = remote(args, 'verify-refresh')['record']
            if document.get('openai', {}).get('type') == 'oauth':
                document['openai'].update(access=fresh['access_token'], accountId=fresh['account_id'], expires=fresh['expires_at'] * 1000, refresh='')
                replace_source(args.auth_file, raw, document)
            print('Native LiteLLM refresh and persistent write verified. OpenCode retains no refresh credential.')
            return 0
        record = token_record(document, time.time())
        if not args.apply:
            print('Handoff source is valid; no changes made.')
            return 0
        require_no_sync()
        remote(args, 'stage', record)
        try:
            document['openai']['refresh'] = ''
            replace_source(args.auth_file, raw, document)
        except Exception:
            remote(args, 'discard')
            raise
        # If activation fails, the durable staged record remains recoverable on
        # the PVC. Do not restore a second refresher or overwrite the destination.
        remote(args, 'activate')
        print('Refresh ownership transferred to LiteLLM. Restart OpenCode with the gateway provider.')
        return 0
    except (HandoffError, OSError, subprocess.TimeoutExpired) as error:
        print(str(error) if isinstance(error, HandoffError) else 'Handoff command could not complete; inspect state before retrying.', file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
