#!/usr/bin/env python3
"""Exercise the real publish workflow against disposable Git remotes and a mock gh."""
import json, os, pathlib, subprocess, tempfile
workflow=json.loads(subprocess.check_output(['ruby','-ryaml','-rjson','-e','puts JSON.generate(YAML.load_file(ARGV[0]))','.github/workflows/publish.yml']))
def run(cmd,cwd,env=None):
 r=subprocess.run(cmd,cwd=cwd,env=env,text=True,capture_output=True)
 assert r.returncode==0, r.stdout+r.stderr
 return r.stdout.strip()
with tempfile.TemporaryDirectory(prefix='peep-publish-retries-') as root:
 root=pathlib.Path(root); seed=root/'seed'; remote=root/'remote.git'; mocks=root/'bin';mocks.mkdir()
 run(['git','init','-b','main',str(seed)],root)
 (seed/'README').write_text('fixture')
 run(['git','add','README'],seed)
 run(['git','-c','user.name=test','-c','user.email=test@example.invalid','commit','-m','seed'],seed)
 run(['git','clone','--bare',str(seed),str(remote)],root)
 mock=mocks/'gh'
 mock.write_text('''#!/usr/bin/env python3
import os,sys,subprocess,pathlib
a=sys.argv[1:]; marker=pathlib.Path(os.environ['PR_MARKER'])
if a[:2]==['auth','setup-git']: pass
elif a[:2]==['repo','clone']: subprocess.run(['git','clone',os.environ['TEST_REMOTE'],a[3]],check=True)
elif a[:2]==['pr','list']: print('https://example.invalid/pr/1' if marker.exists() else '')
elif a[:2]==['pr','create']:
 assert not marker.exists(), 'duplicate PR'
 marker.write_text('created')
 print('https://example.invalid/pr/1')
elif a[:2]==['pr','merge']:
 assert a[2]=='https://example.invalid/pr/1'
 if os.environ.get('BLOCK_MERGE')=='1': sys.exit('merge blocked')
 subprocess.run(['git','--git-dir',os.environ['TEST_REMOTE'],'update-ref','refs/heads/main','refs/heads/peep-1.2.3'],check=True)
 marker.unlink()
else: raise RuntimeError(a)
''');mock.chmod(0o755)
 env=dict(os.environ,PATH=str(mocks)+os.pathsep+os.environ['PATH'],TEST_REMOTE=str(remote),PR_MARKER=str(root/'pr'),GH_TOKEN='fixture',VERSION='1.2.3')
 script=workflow['jobs']['homebrew']['steps'][-1]['run']
 def brew_attempt(label,blocked=False):
  wd=root/label;(wd/'tmp/release').mkdir(parents=True);(wd/'tmp/release/peep.rb').write_text('release formula')
  r=subprocess.run(['bash','-euo','pipefail','-c',script],cwd=wd,env=dict(env,BLOCK_MERGE='1' if blocked else '0'),text=True,capture_output=True)
  assert (r.returncode!=0)==blocked, r.stdout+r.stderr
  if blocked: assert 'merge blocked' in r.stderr
  return r.stdout
 brew_attempt('first',blocked=True)
 head=run(['git','rev-parse','refs/heads/peep-1.2.3'],remote)
 assert 'already has a pull request' in brew_attempt('retry',blocked=True)
 assert run(['git','rev-parse','refs/heads/peep-1.2.3'],remote)==head
 (root/'pr').unlink()
 brew_attempt('recover-pushed-branch',blocked=True);assert (root/'pr').exists()
 brew_attempt('merge-success')
 assert run(['git','rev-parse','refs/heads/main'],remote)==head
 assert not (root/'pr').exists()
 assert 'already contains this release' in brew_attempt('merged')
 # Run the unchanged staging/publishing portion of the AUR job against a local remote.
 aur_script=workflow['jobs']['aur']['steps'][2]['run'];aur_script=aur_script[aur_script.index('cp tmp/release/PKGBUILD'):]
 def aur_attempt(label,content):
  wd=root/label;(wd/'tmp/release').mkdir(parents=True)
  run(['git','clone',str(remote),str(wd/'tmp/aur')],root)
  for name in ['PKGBUILD','peep.SRCINFO']:(wd/'tmp/release'/name).write_text(content)
  return run(['bash','-euo','pipefail','-c',aur_script],wd,dict(env,AUR_EMAIL='test@example.invalid'))
 aur_attempt('aur-first','version 1.2.3')
 aur_head=run(['git','rev-parse','refs/heads/master'],remote)
 run(['git','symbolic-ref','HEAD','refs/heads/master'],remote)
 assert 'already contains this release' in aur_attempt('aur-retry','version 1.2.3')
 assert run(['git','rev-parse','refs/heads/master'],remote)==aur_head
print('PASS: Homebrew blocked merge, open-PR retry, pushed-branch recovery, automated merge, merged-formula retry; AUR first publish and identical retry')
