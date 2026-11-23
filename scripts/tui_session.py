#!/usr/bin/env python3
"""Real PTY acceptance tests. Uses only Python's standard library and synthetic media."""
import errno
import fcntl
import os
from pathlib import Path
import pty
import re
import select
import signal
import struct
import sys
import subprocess
import termios
import time

ROOT = Path(__file__).resolve().parents[1]
ARTIFACTS = ROOT / 'tmp' / 'acceptance'
ARTIFACTS.mkdir(parents=True, exist_ok=True)

class Session:
    def __init__(self, name, *args, env_override=None):
        self.name = name
        self.master, self.slave = pty.openpty()
        self.original = termios.tcgetattr(self.slave)
        self.resize(110, 30, notify=False)
        env = dict(os.environ, TERM='xterm-256color', COLORTERM='truecolor')
        if env_override is not None: env.update(env_override)
        def terminal():
            os.setsid()
            fcntl.ioctl(0, termios.TIOCSCTTY, 0)
        child = "import subprocess,sys,termios; before=termios.tcgetattr(0); p=subprocess.run(sys.argv[1:]); after=termios.tcgetattr(0); mask=~getattr(termios,'PENDIN',0); before[3]&=mask; after[3]&=mask; restored=after==before; print('PEEP_TERMINAL_RESTORED='+str(restored),flush=True); sys.exit(p.returncode if restored else 99)"
        self.proc = subprocess.Popen([sys.executable, '-c', child, env.get('PEEP_TEST_BINARY',str(ROOT/'bin/peep')), '--audio-output=null', '--renderer=cells', *args],
            cwd=ROOT, stdin=self.slave, stdout=self.slave, stderr=self.slave,
            env=env, preexec_fn=terminal)
        self.log = bytearray()
        self.children = set()
    def resize(self, cols, rows, notify=True):
        fcntl.ioctl(self.slave, termios.TIOCSWINSZ, struct.pack('HHHH', rows, cols, 0, 0))
        if notify: os.kill(self.proc.pid, signal.SIGWINCH)
    def read(self, seconds=.2):
        out = bytearray()
        until = time.monotonic()+seconds
        while time.monotonic()<until:
            if select.select([self.master], [], [], max(0, until-time.monotonic()))[0]:
                try: data=os.read(self.master, 262144)
                except OSError as exc:
                    if exc.errno==errno.EIO: break
                    raise
                if not data: break
                out.extend(data); self.log.extend(data)
        return bytes(out)
    def expect(self, needle, timeout=8):
        out=bytearray();until=time.monotonic()+timeout
        while time.monotonic()<until:
            out.extend(self.read(.1))
            if needle in re.sub(rb'\x1b\[[0-?]*[ -/]*[@-~]',b'',out): return bytes(out)
            if self.proc.poll() is not None: break
        raise AssertionError(f'{self.name}: missing {needle!r}; tail={bytes(out[-1800:])!r}')
    def send(self, keys):
        self.read(.15)
        os.write(self.master, keys)
    def record_children(self):
        rows=subprocess.check_output(['ps','-axo','pid=,ppid=']).decode().splitlines()
        edges=[tuple(map(int,line.split())) for line in rows]
        parents={self.proc.pid}
        for _ in range(5):
            parents.update(pid for pid,ppid in edges if ppid in parents)
        self.children |= parents-{self.proc.pid}
    def finish(self):
        self.record_children()
        self.send(b'q')
        self.read(.7)
        self.proc.wait(timeout=5)
        (ARTIFACTS/(self.name+'.ansi')).write_bytes(self.log)
        assert self.proc.returncode==0, bytes(self.log[-1800:])
        assert b'\x1b[?1049l' in self.log, 'alternate screen not restored'
        assert b'PEEP_TERMINAL_RESTORED=True' in self.log, 'terminal attributes not restored'
        for pid in self.children:
            try: os.kill(pid,0)
            except ProcessLookupError: continue
            raise AssertionError(f'child process {pid} survived exit')
        (ARTIFACTS/(self.name+'.ansi')).write_bytes(self.log)
        os.close(self.master);os.close(self.slave)
    def abort(self):
        if self.proc.poll() is None:
            try:
                os.write(self.master, b'\x03')
                self.read(1)
                self.proc.wait(timeout=3)
            except (OSError, subprocess.TimeoutExpired):
                self.record_children()
                for pid in self.children:
                    try: os.kill(pid, signal.SIGTERM)
                    except (ProcessLookupError, PermissionError): pass
                self.proc.kill()
                self.proc.wait(timeout=3)
        (ARTIFACTS/(self.name+'-failure.ansi')).write_bytes(self.log)

