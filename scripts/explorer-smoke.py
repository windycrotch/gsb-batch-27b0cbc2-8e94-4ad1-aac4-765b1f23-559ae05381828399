#!/usr/bin/env python3
"""Focused search, symlinks, glob filtering and sorting in a real terminal."""
import os
import shutil
from tui_session import Session, ROOT
folder=ROOT/'tmp/explorer-fixtures'
if folder.exists():shutil.rmtree(folder)
folder.mkdir(parents=True)
(folder/'rec-folder').mkdir()
(folder/'rec-old.txt').write_text('old\n')
(folder/'rec-new.txt').write_text('new content\n')
os.utime(folder/'rec-old.txt',(1000000000,1000000000))
os.utime(folder/'rec-new.txt',(1700000000,1700000000))
(folder/'alias').symlink_to('rec-folder')
(folder/'missing-link').symlink_to('absent')
(folder/'search.txt').write_text('\n'.join('needle marker '+str(i) if i in (4,40,70) else 'ordinary line '+str(i) for i in range(90)))
s=None
try:
 s=Session('explorer-search-sort',str(folder/'search.txt'))
 s.expect(b'ordinary line');s.read(.2)
 assert b'LINK' in s.log and b'\x1b[38;2;110;210;245m' in s.log
 s.send(b'\r');s.expect(b'READ');s.read(.3)
 s.send(b'/needle');s.expect(b'Match 1/3')
 s.send(b'\r');s.read(.2)
 s.send(b'n');s.expect(b'Match 2/3');s.send(b'n');s.expect(b'Match 3/3')
 s.send(b'n');s.expect(b'Match 1/3');s.send(b'N');s.expect(b'Match 3/3')
 assert b'\x1b[48;2;88;214;190m' in s.log
 s.send(b'\x06\x15absent');s.expect(b'No matches')
 s.send(b'\x1b');s.read(.2);s.send(b'\x1b');s.expect(b'BROWSE')
 s.send(b'/rec-*');s.expect(b'Matches');s.read(.2)
 assert b'3 / 6' in s.log
 s.send(b'\r');s.read(.3);s.send(b'S');s.expect(b'Sort:');s.send(b'm');s.expect(b'Modified')
 s.send(b'S');s.expect(b'Sort:');s.send(b'r');s.expect(b'Modified')
 s.send(b'/\x15*.txt');s.expect(b'Filter: *.txt');s.send(b'\r');s.read(.2)
 s.send(b'S');s.expect(b'Sort:');s.send(b'c');s.expect(b'Created')
 s.send(b'/\x15[');s.expect(b'Invalid glob')
 s.send(b'\x1b');s.read(.2)
 s.resize(60,18);s.read(.3)
 s.finish()
 print('PASS: symlink color/badge, focused search/highlighting/wrap/cancel, glob filtering, sort menu/reversal, invalid pattern, resize and cleanup')
finally:
 if s is not None and s.proc.poll() is None:s.abort()
