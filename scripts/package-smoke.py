#!/usr/bin/env python3
"""Extract, relocate and install a real package; render with an empty tool PATH."""
import hashlib
import os
from pathlib import Path
import platform
import shutil
import struct
import subprocess
import tarfile
from tui_session import Session, ROOT

system={'Darwin':'darwin','Linux':'linux'}[platform.system()]
arch={'arm64':'arm64','aarch64':'arm64','x86_64':'amd64'}[platform.machine()]
archive=ROOT/'dist'/f'peep_dev_{system}_{arch}.tar.gz'
expected=Path(str(archive)+'.sha256').read_text().split()[0]
assert hashlib.sha256(archive.read_bytes()).hexdigest()==expected,'archive checksum mismatch'
fixture=ROOT/'tmp/package-fixtures'
if fixture.exists():shutil.rmtree(fixture)
fixture.mkdir(parents=True)
extracted=fixture/'extracted'
with tarfile.open(archive) as tar:tar.extractall(extracted,filter='data')
relocated=fixture/'relocated package';extracted.rename(relocated)
assert (relocated/'docs/assets/peep-logo.svg').is_file()
assert (relocated/'docs/install.md').is_file()
assert (relocated/'examples/night-shift/04-audiobook/chapter-01.m4b').is_file()
assert (relocated/'examples/night-shift/field-guide-link.md').resolve()==(relocated/'examples/night-shift/00-start-here.md').resolve()

no_tools=fixture/'no-tools';no_tools.mkdir()
env=dict(os.environ,PATH=str(no_tools))
binary=relocated/'bin/peep'
source=b'flowchart LR\nA[Single binary]-->B[Built-in Mermaid]'
result=subprocess.run([str(binary),'__peep_mermaid_png_v1'],input=source,capture_output=True,env=env,timeout=10,check=True)
assert result.stdout.startswith(b'\x89PNG')
w,h=struct.unpack('>II',result.stdout[16:24]);assert 50<=w<=1600 and 50<=h<=1600
prefix=fixture/'install prefix'
subprocess.run([str(binary),'--install','--no-deps','--prefix',str(prefix)],env=env,check=True,capture_output=True)
installed=prefix/'bin/peep'
assert installed.is_symlink()
assert hashlib.sha256(installed.read_bytes()).digest()==hashlib.sha256(binary.read_bytes()).digest()
# The installation must not depend on the extracted package remaining in place.
shutil.rmtree(relocated)
result=subprocess.run([str(installed),'__peep_mermaid_png_v1'],input=source,capture_output=True,env=env,timeout=10,check=True)
assert result.stdout.startswith(b'\x89PNG')
doctor=subprocess.run([str(installed),'--doctor'],capture_output=True,env=env,timeout=10)
assert b'Mermaid: built in' in doctor.stdout and b'mpv: MISSING' in doctor.stdout
assert doctor.returncode==1,'missing media dependencies should be reported'
licenses=subprocess.run([str(installed),'--licenses'],capture_output=True,env=env,check=True)
assert b'MIT License' in licenses.stdout and b'Liberation' in licenses.stdout
# Idempotent self-install: source and destination may already be the same version.
subprocess.run([str(installed),'--install','--no-deps','--prefix',str(prefix)],env=env,check=True,capture_output=True)
(fixture/'diagram.mmd').write_bytes(source)
s=Session('installed-mermaid','--renderer=kitty',str(fixture/'diagram.mmd'),env_override={'PATH':str(no_tools),'PEEP_TEST_BINARY':str(installed)})
try:
 s.expect(b'a=t,f=100',timeout=15)
 s.finish()
finally:
 if s.proc.poll() is None:s.abort()
print('PASS: checksummed archive, relocation, atomic self-install, source removal, no-tool rendering, doctor, notices and installed PTY')
