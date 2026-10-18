"""Read Cockpit's sanitized local connection log; no policy/service changes."""
import json
import os
from pathlib import Path
import re
import subprocess

home = Path.home()
if str(home) != '/Users/znas':
    raise SystemExit('unexpected diagnostic machine account')
command = [str(home / '.local/bin/memql'), 'worker', 'control', '--action=logs']
result = subprocess.run(command, capture_output=True, timeout=20)
if result.returncode:
    raise SystemExit('sanitized local diagnostics unavailable')
value = json.loads(result.stdout)
if value.get('ok') is not True:
    raise SystemExit('sanitized local diagnostics refused')
entries = []
for entry in value.get('logs', []):
    message = entry.get('message', '')
    if not re.search(r'stream|connect|register|cancel|signal|advertis|restart|shutdown', message, re.I):
        continue
    entries.append({k: entry.get(k, '') for k in ('time', 'level', 'message')})
report = {'formatVersion': 1, 'diagnosticOnly': True, 'entries': entries[-50:]}
output = Path('.memql-diagnostics')
output.mkdir(exist_ok=True)
(output / 'stream.json').write_text(json.dumps(report, indent=2) + '\n')
print(json.dumps(report))
