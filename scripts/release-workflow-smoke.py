#!/usr/bin/env python3
"""Run release entry/dispatch steps with a fake GitHub API; never publish a release."""
import json
import os
from pathlib import Path
import subprocess
import tempfile

workflow = json.loads(subprocess.check_output([
    'ruby', '-ryaml', '-rjson', '-e',
    'puts JSON.generate(YAML.load_file(ARGV[0]))', '.github/workflows/release.yml',
]))
jobs = workflow['jobs']
with tempfile.TemporaryDirectory(prefix='peep-release-') as directory:
    root = Path(directory)
    mock = root / 'gh'
    mock.write_text('''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
a = sys.argv[1:]
with open(os.environ['CALLS'], 'a') as f: f.write(json.dumps(a) + '\\n')
if a[0] == 'api' and 'matching-refs' in a[1]:
 print(os.environ.get('EXISTING_SHA', ''))
elif a[:2] == ['api', '--paginate']:
 if os.environ.get('TAGS_ERROR'): sys.exit('tag API unavailable')
 print(os.environ.get('TAGS', ''))
''')
    mock.chmod(0o755)
    env = dict(os.environ, PATH=str(root)+os.pathsep+os.environ['PATH'],
               GITHUB_OUTPUT=str(root/'output'), CALLS=str(root/'calls'),
               EVENT='workflow_dispatch', REF='refs/heads/main',
               VERSION='1.2.3', TAG='main', GH_REPO='test/peep', SHA='abc123')

    def execute(script, overrides=None, success=True):
        result = subprocess.run(['bash', '-euo', 'pipefail', '-c', script],
                                env=env | (overrides or {}), text=True, capture_output=True)
        assert (result.returncode == 0) == success, result.stdout + result.stderr

    version = jobs['version']['steps'][0]['run']
    execute(version)
    assert (root/'output').read_text() == 'version=1.2.3\n'
    execute(version, {'REF': 'refs/heads/topic'}, success=False)
    tags = 'v0.2.0\nv0.9.9\nv0.10.2\nv0.10.1\ndemo-v1\nv9.0.0-rc.1\nv01.2.3'
    for bump, expected in [('patch', '0.10.3'), ('minor', '0.11.0'), ('major', '1.0.0'), ('', '0.10.3')]:
        execute(version, {'VERSION': bump, 'TAGS': tags})
        assert (root/'output').read_text().endswith('version=' + expected + '\n')
    execute(version, {'VERSION': 'patch', 'TAGS': ''})
    assert (root/'output').read_text().endswith('version=0.0.1\n')
    execute(version, {'VERSION': 'patch', 'TAGS_ERROR': '1'}, success=False)

    for invalid in ['v1.2.3', '1.2', '01.2.3', '1.2.3-rc.1', '1.2.3; touch nope']:
        execute(version, {'VERSION': invalid}, success=False)
    execute(version, {'EVENT': 'push', 'TAG': 'v2.3.4'})
    assert (root/'output').read_text().endswith('version=2.3.4\n')

    # Tag creation cannot happen until all build/install jobs succeed.
    assert set(jobs['release']['needs']) == {'version', 'build'}
    tag_step = next(step for step in jobs['release']['steps']
                    if step.get('name') == 'Create immutable tag for manual release')
    assert tag_step['if'] == "github.event_name == 'workflow_dispatch'"
    execute(tag_step['run'], {'TAG': 'v1.2.3'})
    calls = [json.loads(line) for line in (root/'calls').read_text().splitlines()]
    assert calls[-1] == ['api', '--method', 'POST', 'repos/test/peep/git/refs',
                         '-f', 'ref=refs/tags/v1.2.3', '-f', 'sha=abc123']
    for sha, success in [('abc123', True), ('other-commit', False)]:
        (root/'calls').write_text('')
        execute(tag_step['run'], {'TAG': 'v1.2.3', 'EXISTING_SHA': sha}, success=success)
        assert len((root/'calls').read_text().splitlines()) == 1  # no tag mutation

    # Preserve publish.yml as the workflow initiating the OIDC publication.
    assert set(jobs['publish']['needs']) == {'version', 'release'}
    assert jobs['publish']['permissions']['actions'] == 'write'
    (root/'calls').write_text('')
    execute(jobs['publish']['steps'][0]['run'])
    assert json.loads((root/'calls').read_text()) == [
        'workflow', 'run', 'publish.yml', '--repo', 'test/peep', '--ref', 'v1.2.3',
        '-f', 'version=1.2.3', '-f', 'channel=all',
    ]
print('PASS: release version/ref validation, immutable tag creation/retry, publishing dispatch identity')
