#!/usr/bin/env python3
"""Held-key regression through real, asynchronously rendered Markdown assets."""
import os
import re
from tui_session import Session, ROOT

path = ROOT / 'examples/night-shift/field-guide-link.md'
for mode in ('cells', 'kitty'):
    for key, name in ((b'j', 'j'), (b'\x1b[B', 'down')):
        s = Session('document-held-' + mode + '-' + name, '--renderer=' + mode, str(path))
        try:
            s.expect(b'Markdown')
            s.send(b'\r')
            s.expect(b'READ')
            s.read(1)
            positions = []
            for _ in range(180):
                # A held key arrives as repeated events, not one pasted string.
                os.write(s.master, key)
                output = s.read(.04)
                clean = re.sub(rb'\x1b\[[0-?]*[ -/]*[@-~]', b'', output)
                positions.extend((int(at), int(total)) for at, total in re.findall(rb'Markdown \xc2\xb7 (\d+)/(\d+)', clean))
            assert positions, 'no scrolling observed'
            assert all(b[0] >= a[0] for a, b in zip(positions, positions[1:])), positions
            assert positions[-1][0] > 50, ('stuck near first image', positions)
            # At the bottom, End must not advance farther: holding reached it.
            before = positions[-1]
            s.send(b'G')
            output = s.read(.4)
            clean = re.sub(rb'\x1b\[[0-?]*[ -/]*[@-~]', b'', output)
            later = re.findall(rb'Markdown \xc2\xb7 (\d+)/(\d+)', clean)
            assert not later or int(later[-1][0]) == before[0], (before, later)
            if mode == 'kitty':
                assert b'a=t,f=100' in s.log, 'no native image upload'
            s.finish()
            print('PASS:', mode, name, 'reached', before, 'without backward jumps')
        finally:
            if s.proc.poll() is None:
                s.abort()
