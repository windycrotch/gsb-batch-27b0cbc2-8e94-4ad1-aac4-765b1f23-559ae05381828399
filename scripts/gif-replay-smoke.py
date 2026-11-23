#!/usr/bin/env python3
"""GIF replay and directory switching, including fresh native frame payloads."""
import base64
import re
import shutil
from tui_session import Session, ROOT

folder=ROOT/'tmp/gif-replay-fixtures'
for name in ('first','second'):
 (folder/name).mkdir(parents=True,exist_ok=True)
 shutil.copyfile(ROOT/'tmp/fixtures/02-animation.gif',folder/name/(name+'.gif'))

def frames(data):
 result=[]; current=b''
 for header,payload in re.findall(rb'\x1b_G([^;]*);([^\x1b]*)\x1b\\',data):
  if b'a=t' in header:current=b''
  if b'a=t' not in header and not header.startswith(b'm='):continue
  current+=payload
  if b'm=0' in header:
   png=base64.b64decode(current)
   assert png.startswith(b'\x89PNG\r\n\x1a\n')
   result.append(png)
 return result

sessions=[]
try:
 for mode in ('cells','kitty'):
  s=Session('gif-replay-'+mode,'--renderer='+mode,'--play',str(folder/'first/first.gif'));sessions.append(s)
  s.expect(b'[Playing]');s.expect(b'[Ended]')
  for key in (b' ',b'\r'):
   start=len(s.log);s.send(key);s.expect(b'[Playing]');s.expect(b'[Ended]')
   if mode=='kitty':assert len(set(frames(bytes(s.log[start:]))))>1,'replay produced no changing PNG frames'
  s.send(b'\x7f');s.expect(b'Parent directory');s.send(b'/second');s.expect(b'Filter: second');s.send(b'\r');s.send(b'\r');s.expect(b'second.gif')
  start=len(s.log);s.send(b'\r');s.expect(b'[Playing]');s.expect(b'[Ended]')
  if mode=='kitty':assert len(set(frames(bytes(s.log[start:]))))>1,'directory switch produced no changing PNG frames'
  s.finish()
 print('PASS: GIF replay via Space/Enter and navigation to another GIF; fresh native frames; terminal/process cleanup')
finally:
 for s in sessions:
  if s.proc.poll() is None:s.abort()
