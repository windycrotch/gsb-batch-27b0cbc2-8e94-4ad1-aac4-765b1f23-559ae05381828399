#!/usr/bin/env python3
"""Real PTY regression for iTerm 3.7.8beta2's per-chunk graphics replies.

Its KittyImageController.executeTransmit calls respondToTransmit using the final
chunk's verbosity. A missing q on that chunk defaults to a normal i=0;OK reply.
Model that behavior here, including the resulting input delivered to peep.
"""
import os
import time
from tui_session import Session

class ITermSession(Session):
    def __init__(self, name, *args):
        self.pending_output = bytearray()
        self.uploads = 0
        self.acks = 0
        self.multipart = False
        self.displayed = set()
        self.handoffs = 0
        self.blank_transitions = 0
        super().__init__(name, *args, env_override={
            'TERM_PROGRAM': 'iTerm.app', 'TMUX': '', 'STY': '',
        })

    def read(self, seconds=.2):
        data = super().read(seconds)
        self.pending_output.extend(data)
        while True:
            start = self.pending_output.find(b'\x1b_G')
            if start < 0:
                self.pending_output = self.pending_output[-2:]
                break
            end = self.pending_output.find(b'\x1b\\', start)
            if end < 0:
                self.pending_output = self.pending_output[start:]
                break
            command = bytes(self.pending_output[start+3:end])
            del self.pending_output[:end+2]
            header = command.split(b';', 1)[0]
            fields = dict(part.split(b'=', 1) for part in header.split(b',') if b'=' in part)
            action = fields.get(b'a', b't')
            image_id = fields.get(b'i')
            if action == b'q':
                os.write(self.master, b'\x1b_Gi=' + fields[b'i'] + b';OK\x1b\\')
            elif action == b't':
                if not self.multipart:
                    self.uploads += 1
                self.multipart = fields.get(b'm', b'0') == b'1'
                if not self.multipart and fields.get(b'q', b'0') == b'0':
                    self.acks += 1
                    os.write(self.master, b'\x1b_Gi=' + fields.get(b'i', b'0') + b';OK\x1b\\')
            elif action == b'p':
                if self.displayed and image_id not in self.displayed:
                    self.handoffs += 1
                self.displayed.add(image_id)
            elif action == b'd':
                was_visible = bool(self.displayed)
                self.displayed.discard(image_id)
                if was_visible and not self.displayed:
                    self.blank_transitions += 1
        return data

sessions = []
try:
    for mode in ('auto', 'kitty'):
        for path in ('07-detail.jpg', '08-detail.mp4'):
            s = ITermSession('iterm-'+mode+'-'+path, '--renderer='+mode, 'tmp/fixtures/'+path)
            sessions.append(s)
            until = time.monotonic() + 8
            while s.uploads == 0 and time.monotonic() < until:
                s.read(.05)
            assert s.uploads, 'native preview never arrived'
            # Allow decoder completion and any reply-triggered reloads to run.
            for _ in range(20): s.read(.05)
            assert s.uploads == 1, f'{mode}/{path}: preview restarted {s.uploads-1} times after terminal replies'
            assert s.acks == 0, 'image transmission allowed an unsolicited keyboard-input reply'
            assert b'a=p,' in s.log, 'image was not placed'
            if path.endswith('.mp4'):
                s.send(b'\r')
                until = time.monotonic() + 2
                while time.monotonic() < until: s.read(.05)
                assert s.uploads > 2, 'video frames did not advance'
                assert s.acks == 0, 'video uploads generated input replies'
                assert s.handoffs > 1, 'video deleted old frames before placing replacements'
                # Starting playback may hide the static preview once; frame
                # replacement must not repeatedly expose an empty placement set.
                assert s.blank_transitions <= 1, 'blank gaps between video frames'
            s.send(b'?'); s.expect(b'Keyboard & mouse')
            s.send(b'?'); s.read(.2)
            s.finish()
            assert not s.displayed, 'image placements survived exit'
    print('PASS: iTerm-style replies, stable previews, uninterrupted frame handoffs, help and terminal cleanup in auto/kitty modes')
except BaseException:
    for session in sessions:
        if session.proc.poll() is None: session.abort()
    raise
