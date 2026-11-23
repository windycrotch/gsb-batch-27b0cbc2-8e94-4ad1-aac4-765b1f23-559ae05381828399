#!/usr/bin/env python3
"""Built-in Mermaid integration through the native terminal protocol."""
import base64
import os
from pathlib import Path
import re
import shutil
import struct
from tui_session import Session, ROOT, ARTIFACTS

folder=ROOT/'tmp/mermaid-fixtures'
folder.mkdir(exist_ok=True)
(folder/'diagram.mmd').write_text('flowchart LR\n A[Browse] --> B{Media?}\n B --> C[Preview]\n')
(folder/'README.md').write_text('# Diagram preview\n\n```mermaid\nsequenceDiagram\n Alice->>Bob: Hello\n Bob-->>Alice: Hi\n```\n\nFollowing paragraph.\n')
(folder/'invalid.mmd').write_text('This is not a diagram.\n')
# The compiled-in renderer must work with no helper or toolchain on PATH.
missing=folder/'without-mermaid';missing.mkdir(exist_ok=True)

def first_png(data):
 encoded=b'';active=False
 for header,payload in re.findall(rb'\x1b_G([^;]*);([^\x1b]*)\x1b\\',data):
  if b'a=t' in header:encoded=b'';active=True
  if not active:continue
  encoded+=payload
  if b'm=0' in header:return base64.b64decode(encoded)
 raise AssertionError('no complete PNG')

sessions=[]
try:
 for name in ('diagram.mmd','README.md'):
  s=Session('mermaid-'+name,'--renderer=kitty',str(folder/name));sessions.append(s)
  s.expect(b'a=t,f=100',timeout=15);s.read(.3)
  png=first_png(bytes(s.log));assert png.startswith(b'\x89PNG\r\n\x1a\n')
  w,h=struct.unpack('>II',png[16:24]);assert w>=100 and h>=100
  (ARTIFACTS/(name+'.png')).write_bytes(png)
  s.send(b'\r');s.expect(b'READ');s.send(b'R');s.expect(b'Source');s.send(b'R');s.expect(b'a=t,f=100',timeout=10)
  s.resize(80,22);s.read(.5);s.send(b'?');s.read(.2);s.send(b'?');s.read(.3);s.finish()
 s=Session('mermaid-cells',str(folder/'diagram.mmd'));sessions.append(s)
 s.expect(b'diagram needs native graphics');assert b'flowchart LR' in s.log;s.finish()
 s=Session('mermaid-missing','--renderer=kitty',str(folder/'diagram.mmd'),env_override={'PATH':str(missing)});sessions.append(s)
 s.expect(b'a=t,f=100',timeout=15);s.finish()
 s=Session('mermaid-invalid','--renderer=kitty',str(folder/'invalid.mmd'));sessions.append(s)
 s.expect(b'Mermaid render failed',timeout=15);assert b'This is not a diagram.' in s.log;s.finish()
 print('PASS: standalone/fenced Mermaid PNGs, source toggle, resize/overlay, no external tools, cells and syntax failure fallbacks')
finally:
 for s in sessions:
  if s.proc.poll() is None:s.abort()
