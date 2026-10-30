"""Load plugin in an isolated copy of the installed host; no real auth files."""
import argparse
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.request

root = Path(__file__).resolve().parents[1]
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--host', default=os.environ.get('CLIPROXYAPI_BINARY', shutil.which('cli-proxy-api')),
                    help='Path to the CLIProxyAPI binary (or set CLIPROXYAPI_BINARY).')
args = parser.parse_args()
if not args.host:
    parser.error('provide --host, set CLIPROXYAPI_BINARY, or put cli-proxy-api on PATH')
host = str(Path(args.host).expanduser().resolve())
if not Path(host).is_file():
    parser.error('CLIProxyAPI binary does not exist: ' + host)
with tempfile.TemporaryDirectory(prefix='autoreset-host-') as folder:
    p = Path(folder)
    (p/'auth').mkdir()
    (p/'plugins').mkdir()
    shutil.copy2(root/'dist/codex-autoreset.so', p/'plugins/codex-autoreset.so')
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        port = s.getsockname()[1]
    key = secrets.token_hex(24)
    config = f'''config-version: 8
server:
  host: 127.0.0.1
  port: {port}
management:
  allow-remote: false
  secret-key: {key}
  disable-control-panel: true
  disable-auto-update-panel: true
auth:
  dir: {p / 'auth'}
plugins:
  enabled: true
  dir: {p / 'plugins'}
  configs:
    codex-autoreset:
      enabled: true
      dry_run: true
      state_file: {p / 'state.json'}
'''
    # v8 auth files directory is configured by auth-dir legacy alias too.
    config += f'auth-dir: {p / "auth"}\n'
    (p/'config.yaml').write_text(config)
    env = dict(os.environ, HOME=str(p))
    with (p/'output.log').open('w+') as log:
        proc = subprocess.Popen([host, '-config', str(p/'config.yaml')], cwd=p, env=env, stdout=log, stderr=log)
        try:
            for _ in range(20):
                if proc.poll() is not None:
                    log.seek(0)
                    raise RuntimeError('isolated host exited: '+log.read()[-6000:])
                try:
                    req = urllib.request.Request(f'http://127.0.0.1:{port}/v0/resource/plugins/codex-autoreset/status', headers={'Authorization':'Bearer '+key})
                    result = json.load(urllib.request.urlopen(req, timeout=2))
                    assert result['enabled'] is True and result['dry_run'] is True, result
                    print('PASS: installed host loaded plugin, management status enabled, dry_run=true, empty auth directory')
                    break
                except (OSError, ValueError):
                    time.sleep(.1)
            else:
                log.seek(0)
                raise RuntimeError('plugin status unavailable: '+log.read()[:10000])
        finally:
            proc.terminate()
            try:
                proc.wait(timeout=8)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
