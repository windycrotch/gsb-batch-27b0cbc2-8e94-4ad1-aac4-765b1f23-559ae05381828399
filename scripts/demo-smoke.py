#!/usr/bin/env python3
"""Validate the committed demo collection and exercise it through peep's PTY."""
import json
import re
import shutil
import subprocess
from pathlib import Path
from tui_session import Session, ROOT

root = ROOT / 'examples/night-shift'
media = list((root/'01-images').glob('*')) + list((root/'02-motion').glob('*')) + list((root/'03-music').glob('*')) + list((root/'04-audiobook').glob('*.m4b'))
for path in media:
    info = json.loads(subprocess.check_output(['ffprobe', '-v', 'error', '-show_streams', '-show_format', '-of', 'json', str(path)]))
    assert info['streams'], path
    if path.suffix in ('.m4a', '.m4b', '.mp4'):
        assert float(info['format']['duration']) > 20, path
        assert any(s['codec_type']=='audio' for s in info['streams']), path
    subprocess.run(['ffmpeg','-v','error','-i',str(path),'-f','null','-'],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.PIPE,timeout=45)
for path in (root/'05-diagrams').glob('*.mmd'):
    result = subprocess.run([str(ROOT/'bin/peep'),'__peep_mermaid_png_v1'],input=path.read_bytes(),capture_output=True,check=True,timeout=10)
    assert result.stdout.startswith(b'\x89PNG'), path
assert (root/'field-guide-link.md').resolve()==root/'00-start-here.md'
assert (root/'images-link').resolve()==root/'01-images'
s=None
try:
    launcher=ROOT/'tmp/demo-launcher'
    launcher.write_text('#!/bin/sh\nexec just demo\n')
    launcher.chmod(0o700)
    s=Session('demo-recipe',env_override={'PEEP_TEST_BINARY':str(launcher)})
    s.expect(b'Night Shift');s.read(.3)
    workspace=Path(re.search(rb'Demo workspace: ([^\r\n]+)',s.log).group(1).decode())
    assert workspace.is_relative_to(ROOT/'tmp')
    assert (workspace/'field-guide-link.md').is_symlink()
    assert (workspace/'01-images/01-moonrise.jpg').stat().st_mtime < (workspace/'01-images/02-daybreak.png').stat().st_mtime
    assert (workspace/'04-audiobook/chapter-01.m4b').read_bytes()==(root/'04-audiobook/chapter-01.m4b').read_bytes()
    s.finish()
    shutil.rmtree(workspace)

    s=Session('demo-audiobook','--play',str(root/'04-audiobook/chapter-01.m4b'),str(root/'04-audiobook/chapter-02.m4b'))
    s.expect(b'[Playing]');s.send(b' ');s.expect(b'[Paused]')
    s.send(b']');s.read(.5);s.send(b'n');s.expect(b'chapter-02.m4b');s.finish()
    s=Session('demo-music','--play',str(root/'03-music/01-night-shift.m4a'))
    s.expect(b'[Playing]');s.send(b'v');s.read(1);s.send(b' ');s.expect(b'[Paused]');s.finish()
    s=Session('demo-field-guide','--renderer=kitty',str(root/'00-start-here.md'))
    s.expect(b'Night Shift');s.send(b'\r');s.expect(b'READ');s.read(2)
    assert b'a=t,f=100' in s.log, 'no native PNG upload'
    s.send(b'/signal');s.expect(b'Match');s.send(b'\r');s.read(.2);s.send(b'n');s.read(.2)
    s.send(b'R');s.expect(b'Source');s.finish()
    print('PASS: disposable demo recipe, demo streams decode, all diagrams render, audiobook playback/seek/next, music, illustrated Markdown/search/source, cleanup')
finally:
    if s is not None and s.proc.poll() is None:s.abort()
