#!/usr/bin/env python3
"""Document previews in a real PTY. Synthetic files only."""
import os
import re
import shutil
import struct
import zlib
from tui_session import Session, ROOT, ARTIFACTS

folder=ROOT/'tmp/document-fixtures'
folder.mkdir(exist_ok=True)
(folder/'README.md').write_text('# Document preview\n\nA **bold** paragraph.\n\n'+''.join(f'## Section {i}\n\nBody {i}: readable text.\n\n' for i in range(80)))
(folder/'binary.dat').write_bytes(b'\x00\xff\x00')
(folder/'notes').write_text('Plain text without an extension.\n')
fifo=folder/'special.md'
if not fifo.exists():os.mkfifo(fifo)
shutil.copyfile(ROOT/'tmp/fixtures/04-audio.wav',folder/'song.wav')
def chunk(kind,data):
 return struct.pack('>I',len(data))+kind+data+struct.pack('>I',zlib.crc32(kind+data))
pixels=b''.join(b'\x00'+b''.join(bytes((x%256,100,200)) for x in range(320)) for _ in range(32))
(folder/'image.png').write_bytes(b'\x89PNG\r\n\x1a\n'+chunk(b'IHDR',struct.pack('>IIBBBBB',320,32,8,2,0,0,0))+chunk(b'IDAT',zlib.compress(pixels))+chunk(b'IEND',b''))
(folder/'images.md').write_text('# Embedded images\n\n![First](image.png)\n\n![Second][photo]\n\n[photo]: image.png\n\n'+('After images.\n\n'*40))
(folder/'missing.md').write_text('![missing](absent.png)\n\n![remote](https://example.invalid/image.png)')
sessions=[]
try:
 s=Session('document',str(folder/'README.md'));sessions.append(s)
 s.expect(b'Markdown');s.send(b'\r');s.expect(b'READ')
 s.send(b'\x1b[6~');s.read(.3)
 s.send(b'R');s.expect(b'Source');s.send(b'R');s.expect(b'Markdown')
 s.resize(80,22);s.read(.3);s.send(b'\x1b');s.expect(b'BROWSE');s.finish()
 s=Session('document-binary',str(folder/'binary.dat'));sessions.append(s)
 s.expect(b'binary or non-UTF-8');s.finish()
 s=Session('document-special',str(fifo));sessions.append(s)
 s.expect(b'no preview for special files');s.finish()
 s=Session('document-audio','--play',str(folder/'song.wav'));sessions.append(s)
 s.expect(b'[Playing]');s.send(b'/README');s.expect(b'Filter: README');s.send(b'\r');s.send(b'\r');s.expect(b'READ')
 s.send(b' ');s.expect(b'[Paused]');s.send(b']');s.read(.4)
 clean=re.sub(rb'\x1b\[[0-?]*[ -/]*[@-~]',b'',s.log)
 assert b'song.wav' in clean and b'Markdown' in clean
 s.send(b'\x1b');s.read(.2);s.finish()
 for mode in ('cells','kitty'):
  s=Session('document-images-'+mode,'--renderer='+mode,str(folder/'images.md'));sessions.append(s)
  s.expect(b'a=t,f=100' if mode=='kitty' else b'\xe2\x96\x80');s.read(.5)
  if mode=='kitty':assert bytes(s.log).count(b'a=t,f=100')>=2,'multiple images missing'
  s.send(b'\r');s.expect(b'READ');s.send(b'\x1b[6~');s.read(.4)
  if mode=='kitty':assert b'y=' in s.log,'crop placement missing'
  s.send(b'R');s.expect(b'Source');s.send(b'R');s.expect(b'Markdown');s.resize(80,22);s.read(.5)
  s.send(b'?');s.read(.2);s.send(b'?');s.read(.3);s.finish()
 s=Session('document-image-errors',str(folder/'missing.md'));sessions.append(s)
 s.expect(b'remote images are not fetched');s.read(.3)
 assert b'no such file' in s.log,'missing file error absent'
 s.finish()
 print('PASS: Markdown focus/scroll/source/resize, binary/FIFO fallback, and audio transport while reading')
finally:
 for s in sessions:
  if s.proc.poll() is None:s.abort()
