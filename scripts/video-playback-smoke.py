#!/usr/bin/env python3
"""Native FPS, playback clock, signal cleanup and interactive-editor PTY checks."""
import base64
import json
import os
import re
import signal
import socket
import statistics
import struct
import subprocess
import sys
import time
import zlib
from tui_session import Session, ROOT, ARTIFACTS

folder = ROOT / 'tmp/video-playback-fixtures'
folder.mkdir(parents=True, exist_ok=True)
video = folder / 'clock.mkv'
# Lossless RGB: the first pixel records source time in tenths of a second.
subprocess.run(['ffmpeg', '-v', 'error', '-y', '-f', 'lavfi', '-i',
    "testsrc2=size=1280x720:rate=24:duration=16,format=gbrp,geq=r='if(lt(X,8)*lt(Y,8),floor(T*10),r(X,Y))':g='g(X,Y)':b='b(X,Y)'",
    '-f', 'lavfi', '-i', 'sine=frequency=440:duration=16',
    '-c:v', 'ffv1', '-c:a', 'pcm_s16le', '-shortest', str(video)], check=True)

def processes():
    return [line.strip().split(None, 2) for line in subprocess.check_output(
        ['ps', '-axo', 'pid=,ppid=,command=']).decode().splitlines()]

def peep_pid(s):
    return next(int(pid) for pid, parent, _ in processes() if int(parent) == s.proc.pid)

def connect_clock(s):
    parent = peep_pid(s)
    command = next(command for _, ppid, command in processes()
                   if int(ppid) == parent and '--input-ipc-server=' in command)
    path = re.search(r'--input-ipc-server=(\S+)', command).group(1)
    conn = socket.socket(socket.AF_UNIX)
    conn.settimeout(2)
    conn.connect(path)
    return conn, conn.makefile('rb')

def clock(conn, reader):
    conn.sendall(b'{"command":["get_property","time-pos"],"request_id":1}\n')
    while True:
        reply = json.loads(reader.readline())
        if reply.get('request_id') == 1:
            assert reply['error'] == 'success', reply
            return reply['data']

def png_time(png):
    assert png[:8] == b'\x89PNG\r\n\x1a\n'
    assert png[24:26] == bytes([8, 2]), 'fixture must remain 8-bit RGB'
    offset, compressed = 8, bytearray()
    while offset < len(png):
        size = struct.unpack('!I', png[offset:offset+4])[0]
        if png[offset+4:offset+8] == b'IDAT':
            compressed.extend(png[offset+8:offset+8+size])
        offset += size + 12
    # First row/first pixel has no left/up neighbor, so every PNG filter agrees.
    return zlib.decompress(compressed)[1] / 10

class Frames:
    def __init__(self):
        self.pending, self.payload = b'', b''
    def feed(self, data):
        self.pending += data
        result = []
        while True:
            start = self.pending.find(b'\x1b_G')
            if start < 0:
                self.pending = self.pending[-3:]
                break
            end = self.pending.find(b'\x1b\\', start)
            if end < 0:
                self.pending = self.pending[start:]
                break
            command = self.pending[start+3:end]
            self.pending = self.pending[end+2:]
            header, sep, payload = command.partition(b';')
            if not sep or b'm=' not in header:
                continue
            if b'a=t' in header:
                self.payload = b''
            self.payload += payload
            if b'm=0' in header:
                result.append(png_time(base64.b64decode(self.payload)))
                self.payload = b''
        return result

def verify_exit(s):
    deadline = time.monotonic() + 6
    while s.proc.poll() is None and time.monotonic() < deadline:
        s.read(.1)
    s.read(.1)
    s.proc.wait(timeout=1)
    (ARTIFACTS / (s.name + '.ansi')).write_bytes(s.log)
    assert s.proc.returncode == 0, bytes(s.log[-1800:])
    assert b'PEEP_TERMINAL_RESTORED=True' in s.log, 'termios not restored'
    for mode in (b'1049', b'2026', b'1000', b'1002', b'1003', b'1006', b'1004', b'2004'):
        assert b'\x1b[?' + mode + b'l' in s.log, ('mode not cleared', mode)
    for pid in s.children:
        try: os.kill(pid, 0)
        except ProcessLookupError: continue
        raise AssertionError(f'owned child {pid} survived exit')
    os.close(s.master)
    os.close(s.slave)

s = None
try:
    for invalid in ('0', '61'):
        p = subprocess.run([str(ROOT/'bin/peep'), '--fps='+invalid], capture_output=True)
        assert p.returncode != 0 and b'--fps must be between' in p.stderr
    s = Session('video-clock', '--renderer=kitty', '--fps=12', '--play', str(video))
    s.expect(b'[Playing]')
    conn, reader = connect_clock(s)
    frames = Frames()
    frames.feed(s.log)
    lags, recovered = [], []
    start = time.monotonic()
    changed = stalled = False
    while time.monotonic() - start < 9:
        elapsed = time.monotonic() - start
        if elapsed > 3 and not changed:
            os.write(s.master, b'F')
            changed = True
        if elapsed > 5 and not stalled:
            # Backpressure the real PTY, then allow the scheduler to recover.
            time.sleep(1.1)
            stalled = True
        times = frames.feed(s.read(.03))
        if times and elapsed > 1.5:
            lag = clock(conn, reader) - times[-1]
            if 1.5 < elapsed < 5:
                lags.append(lag)
            if elapsed > 7:
                recovered.append(lag)
    reader.close(); conn.close()
    assert len(lags) >= 15 and len(recovered) >= 8, (len(lags), len(recovered))
    assert statistics.median(lags) < .35 and max(lags) < .8, lags
    assert statistics.median(recovered) < .35 and max(recovered) < .8, recovered
    clean = re.sub(rb'\x1b\[[0-?]*[ -/]*[@-~]', b'', s.log)
    assert b'/12 FPS sent/target' in clean and b'/24 FPS sent/target' in clean
    s.finish()
    print(f'PASS: native clock median lag {statistics.median(lags):.3f}s; after PTY stall {statistics.median(recovered):.3f}s; live 12→24 FPS')

    for sig in (None, signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
        name = 'ctrl-c' if sig is None else signal.Signals(sig).name
        s = Session('video-exit-'+name, '--renderer=kitty', '--play', str(video))
        s.expect(b'[Playing]'); s.read(.5); s.record_children()
        if sig is None: os.write(s.master, b'\x03')
        else: os.kill(peep_pid(s), sig)
        verify_exit(s)
        print('PASS: video', name, 'restores terminal and stops owned children')

    editor = folder / 'interactive-editor'
    editor.write_text('#!'+sys.executable+'\nimport os,sys\nassert all(os.isatty(fd) for fd in (0,1,2)), "EDITOR_LOST_TTY"\nprint("EDITOR_TTY_READY", flush=True)\nassert input()=="return"\n')
    editor.chmod(0o700)
    document = folder / 'note.txt'
    document.write_text('Editor handoff fixture\n')
    for interrupt in (False, True):
        s = Session('editor-tty-'+str(interrupt), '--renderer=auto', str(document),
                    env_override={'VISUAL':str(editor), 'TMUX':'', 'STY':''})
        # Auto wraps both input and output even when the query is unanswered.
        s.expect(b'Editor handoff fixture'); s.read(.5)
        s.send(b'e'); s.expect(b'EDITOR_TTY_READY'); s.record_children()
        if interrupt:
            os.kill(peep_pid(s), signal.SIGTERM)
            verify_exit(s)
        else:
            s.send(b'return\r'); s.expect(b'Editor closed'); s.finish()
        print('PASS: editor receives real TTY;', 'SIGTERM cleanup' if interrupt else 'interactive return')
finally:
    if s is not None and s.proc.poll() is None:
        s.abort()
