#!/usr/bin/env python3
"""Exercise source upgrades in isolated homes/PATHs, without media setup."""
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile

root = Path(__file__).resolve().parent.parent
binary = root / 'bin/peep'
expected = hashlib.sha256(binary.read_bytes()).digest()
built_version = subprocess.check_output([str(binary), '--version'], text=True)

with tempfile.TemporaryDirectory(prefix='peep-install-') as tmp:
    for case in ('gopath', 'gobin', 'fresh', 'conflict'):
        home = Path(tmp) / case
        tools = home / 'tools'
        tools.mkdir(parents=True)
        go_bin = home / ('custom executables' if case == 'gobin' else 'go/bin')
        local_bin = home / '.local/bin'
        other_bin = home / 'other/bin'
        for directory in (go_bin, local_bin, other_bin):
            directory.mkdir(parents=True)
        # Source installation must honor go env, including a persisted custom
        # GOBIN and the first entry of a multi-entry GOPATH.
        go = tools / 'go'
        go.write_text('#!/bin/sh\ncase "$2" in\nGOBIN) printf "%s\\n" "$TEST_GOBIN";;\nGOPATH) printf "%s\\n" "$TEST_GOPATH";;\nesac\n')
        go.chmod(0o755)
        old = go_bin / 'peep' if case in ('gopath', 'gobin') else other_bin / 'peep'
        if case != 'fresh':
            old.write_text('#!/bin/sh\necho old-version\n')
            old.chmod(0o755)
        env = dict(os.environ, HOME=str(home),
                   PATH=os.pathsep.join(map(str, (tools, go_bin, other_bin, local_bin))) + ':/usr/bin:/bin',
                   TEST_GOBIN=str(go_bin) if case == 'gobin' else '',
                   TEST_GOPATH=str(home / 'go') + ':' + str(home / 'second-go'))
        result = subprocess.run(['/bin/bash', 'scripts/install.sh', '--no-deps'],
                                cwd=root, env=env, text=True, capture_output=True, check=True)
        installed = (go_bin if case in ('gopath', 'gobin') else local_bin) / 'peep'
        assert installed.is_symlink(), (case, result.stdout, result.stderr)
        assert hashlib.sha256(installed.read_bytes()).digest() == expected, case
        command = subprocess.run(['/bin/bash', '-c', 'peep --version'], env=env,
                                 text=True, capture_output=True, check=True)
        if case == 'conflict':
            assert command.stdout.strip() == 'old-version'
            assert 'WARNING: PATH still selects' in result.stdout
            assert str(old) in result.stdout
            assert not old.is_symlink()
        else:
            assert command.stdout == built_version, command.stdout
            assert 'PATH selects the installed executable:' in result.stdout
        # Re-running an upgrade must work with the new symlink already active.
        subprocess.run(['/bin/bash', 'scripts/install.sh', '--no-deps'], cwd=root,
                       env=env, check=True, capture_output=True)
        assert hashlib.sha256(installed.read_bytes()).digest() == expected
        print(f'PASS: {case}')

    missing_path = dict(os.environ, PATH=str(Path(tmp) / 'empty'))
    result = subprocess.run([str(binary), '--install', '--no-deps', '--prefix', str(Path(tmp) / 'offline')],
                            env=missing_path, text=True, capture_output=True, check=True)
    assert 'to the front of PATH' in result.stdout
    print('PASS: installation without tools or peep on PATH')
