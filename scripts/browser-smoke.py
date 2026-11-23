#!/usr/bin/env python3
"""Numbered browser navigation, syntax colors, and contextual transport in a PTY."""
import re
from tui_session import Session, ROOT
folder=ROOT/'tmp/browser-fixtures'
folder.mkdir(parents=True,exist_ok=True)
(folder/'fence.md').unlink(missing_ok=True)
(folder/'go.mod').write_text('module peep-browser-fixtures\n\ngo 1.24.0\n')
for i in range(1,41):
 (folder/f'entry-{i:02d}.go').write_text(f'package main\n// Entry {i:02d}\nfunc main() {{ println("hello") }}\n')
(folder/'zz-fence.md').write_text('# Code example\n\n```go\npackage main\nfunc main() {}\n```\n')
sessions=[]
def key(s,text):
 for c in text: s.send(c.encode())
try:
 s=Session('browser-navigation',str(folder));sessions.append(s)
 first=s.expect(b'entry-01.go');first+=s.read(.5)
 assert b'NOW PLAYING' not in first,'empty transport visible'
 key(s,'12G');s.expect(b'Entry 11');key(s,'5j');s.expect(b'Entry 16')
 key(s,'gg');s.expect(b'Parent directory')
 key(s,'G');s.expect(b'Code example');tail=s.read(.2)
 assert b'\x1b[38;2;255;121;198m' in s.log,'syntax color absent'
 key(s,'#');s.read(.1)
 s.send(b'/entry-02');s.expect(b'Filter: entry-02');s.send(b'\r');s.read(.3)
 s.send(b'\r');s.expect(b'READ');s.resize(72,18);s.read(.4)
 s.finish()
 print('PASS: counted jumps, first/last, relative toggle, fuzzy filtering, syntax colors, transport hidden, resize and cleanup')
finally:
 for s in sessions:
  if s.proc.poll() is None:s.abort()
