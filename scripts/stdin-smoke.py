#!/usr/bin/env python3
"""Pipe search results into a real TUI; keyboard and editors must retain the TTY."""
import os
import pty
import re
import shutil
import subprocess
import sys
import time
from tui_session import Session, ROOT, ARTIFACTS

folder = ROOT / 'tmp/stdin-fixtures'
if folder.exists():
    shutil.rmtree(folder)
folder.mkdir(parents=True)
for name, text in {'a/shared.md': 'ALPHA PEEP_MATCH\n', 'z/shared.md': 'ZULU PEEP_MATCH\n',
                   'unrelated.txt': 'Not a match\n', 'space name.txt': 'SPACE_CONTENT\n',
                   'line\nbreak.txt': 'NEWLINE_CONTENT\n', '.hidden': 'HIDDEN_CONTENT\n'}.items():
    path = folder / name
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)

editor = folder / 'editor'
editor.write_text('#!' + sys.executable + '\nimport os,sys\n'
                  'assert all(os.isatty(fd) for fd in (0,1,2)), "EDITOR_LOST_TTY"\n'
                  'print("PIPE_EDITOR_READY", flush=True)\n'
                  'assert input()=="return"\n'
                  'with open(sys.argv[1], "a") as f: f.write("EDIT_SAVED\\n")\n')
editor.chmod(0o700)

sessions = []
def start(name, payload=b'', args=(), rg=False):
    launcher = folder / ('launch-' + name)
    code = '#!' + sys.executable + '\nimport subprocess,sys\n'
    command = [str(ROOT / 'bin/peep')]
    if rg:
        code += "search=subprocess.Popen(['rg','--sort','path','-l','PEEP_MATCH','tmp/stdin-fixtures/a','tmp/stdin-fixtures/z'],stdout=subprocess.PIPE)\n"
        code += f'p=subprocess.Popen({command!r}+sys.argv[1:],stdin=search.stdout)\nsearch.stdout.close()\nstatus=p.wait()\nassert search.wait()==0\n'
    else:
        code += f'p=subprocess.run({command!r}+sys.argv[1:],input={payload!r})\nstatus=p.returncode\n'
    launcher.write_text(code + 'sys.exit(status)\n')
    launcher.chmod(0o700)
    session = Session('stdin-' + name, *args, env_override={'PEEP_TEST_BINARY': str(launcher), 'VISUAL': str(editor),
                      'TERM_PROGRAM': 'unknown', 'KITTY_WINDOW_ID': '', 'TMUX': '', 'STY': ''})
    sessions.append(session)
    return session

def exited(s, code):
    # Bubble Tea's package initializer can wait for terminal-color probes even
    # when run() exits before starting a TUI. Keep draining the PTY during it.
    deadline = time.monotonic() + 15
    while s.proc.poll() is None and time.monotonic() < deadline:
        s.read(.1)
    s.proc.wait(timeout=1)
    s.read(.2)
    assert s.proc.returncode == code, bytes(s.log[-2000:])
    assert b'PEEP_TERMINAL_RESTORED=True' in s.log
    (ARTIFACTS / (s.name + '.ansi')).write_bytes(s.log)
    os.close(s.master)
    os.close(s.slave)

try:
    s = start('rg', rg=True)
    s.expect(b'ALPHA PEEP_MATCH')
    assert 'Results · 2'.encode() in s.log
    assert b'Input order' in s.log and b'unrelated.txt' not in s.log
    s.send(b'j'); s.expect(b'ZULU PEEP_MATCH')
    s.send(b'\r'); s.expect(b'READ')
    s.send(b'/PEEP_MATCH'); s.expect(b'Find in file:')
    s.send(b'\x1b'); s.read(.2)
    s.send(b'e'); s.expect(b'PIPE_EDITOR_READY')
    s.send(b'return\r'); s.expect(b'Editor closed')
    assert 'EDIT_SAVED' in (folder / 'z/shared.md').read_text()
    s.finish()

    s = start('probe', b'tmp/stdin-fixtures/a/shared.md\n', ('--renderer=auto',))
    data = s.expect(b'a=q')
    query = re.search(rb'\x1b_Gi=(\d+),s=1,v=1,a=q', data).group(1)
    os.write(s.master, b'\x1b_Gi=' + query + b';EINVAL: unsupported\x1b\\')
    if b'ALPHA PEEP_MATCH' not in re.sub(rb'\x1b\[[0-?]*[ -/]*[@-~]', b'', s.log):
        s.expect(b'ALPHA PEEP_MATCH')
    s.send(b'/shared'); s.expect(b'Matches')
    s.send(b'\x1b'); s.read(.2)
    s.finish()

    # Exact paths, including whitespace, hidden files, and embedded newlines.
    names = ['space name.txt', 'line\nbreak.txt', '.hidden']
    payload = b'\0'.join(os.fsencode(folder / name) for name in names) + b'\0'
    s = start('nul', payload, ('--read0',))
    s.expect(b'SPACE_CONTENT')
    s.send(b'j'); s.expect(b'NEWLINE_CONTENT')
    s.send(b'j'); s.expect(b'HIDDEN_CONTENT')
    s.send(b'H'); s.expect(b'all supplied paths')
    s.finish()

    # Input order differs from the normal alphabetical directory listing.
    s = start('order', b'tmp/stdin-fixtures/z/shared.md\ntmp/stdin-fixtures/a/shared.md\n')
    s.expect(b'ZULU PEEP_MATCH')
    s.send(b'j'); s.expect(b'ALPHA PEEP_MATCH')
    s.send(b'/shared'); s.expect(b'Matches')
    s.send(b'\x1b'); s.expect(b'Input order')
    s.finish()

    s = start('directory', os.fsencode(folder / 'a') + b'\n')
    s.expect(b'RESULTS')
    s.send(b'\r'); s.expect(b'Backspace results')
    s.send(b'\x7f'); s.expect(b'RESULTS')
    s.finish()

    for name, payload, args, message in (
        ('empty', b'', (), None),
        ('missing', b'no-such-peep-stdin-file\n', (), b'stdin path'),
        ('mixed', b'tmp/stdin-fixtures/a/shared.md\n', ('README.md',), b'not both'),
        ('nul-without-flag', b'file\0', (), b'use --read0'),
    ):
        s = start(name, payload, args)
        if message:
            s.expect(message)
        exited(s, 1 if message else 0)
        if name == 'empty':
            assert b'\x1b[?1049h' not in s.log, 'empty results opened the TUI'
    # A pipe still needs a controlling terminal; stdout being a TTY is not enough.
    master, slave = pty.openpty()
    try:
        result = subprocess.run([str(ROOT / 'bin/peep')], input=b'README.md\n',
                                stdout=slave, stderr=subprocess.PIPE, start_new_session=True, timeout=15)
        assert result.returncode != 0 and b'open terminal for keyboard input' in result.stderr, result.stderr
    finally:
        os.close(master)
        os.close(slave)
    result = subprocess.run([str(ROOT / 'bin/peep')], input=b'README.md\n', capture_output=True, timeout=15)
    assert result.returncode != 0 and b'interactive terminal' in result.stderr
    print('PASS: rg pipeline, input order, documents/search, editor TTY, graphics probe, NUL filenames, directory return, empty/invalid input, no-TTY errors, terminal cleanup')
finally:
    for s in sessions:
        if s.proc.poll() is None:
            s.abort()
