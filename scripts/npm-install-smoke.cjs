#!/usr/bin/env node
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const {spawnSync} = require('node:child_process');
const root = path.resolve(process.argv[2] || 'dist/distribution');
const m = JSON.parse(fs.readFileSync(path.join(root, 'npm/peep/package.json')));
const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'peep-npm-'));
function run(command, args, options = {}) {
  const result = spawnSync(command, args, {encoding: 'utf8', timeout: 120000, ...options});
  assert.equal(result.status, 0, `${command}: ${result.error || result.stderr || result.stdout}`);
  return result.stdout;
}
try {
  const stem = m.name.replace(/^@/, '').replace('/', '-');
  const files = [`${stem}-${process.platform}-${process.arch}-${m.version}.tgz`, `${stem}-${m.version}.tgz`];
  run('npm', ['install', '--prefix', tmp, '--cache', path.join(tmp, 'cache'), '--offline', '--ignore-scripts', '--no-audit', '--no-fund', ...files.map(f => path.join(root, 'npm-tarballs', f))]);
  const executable = path.join(tmp, 'node_modules/.bin/peep');
  assert.match(run(executable, ['--version']), new RegExp(`peep ${m.version.replaceAll('.', '\\.')}`));
  const image = run(executable, ['__peep_mermaid_png_v1'], {input:'flowchart LR\nA[peep]-->B[Mermaid]\n', encoding:null});
  assert.deepEqual(image.subarray(0, 8), Buffer.from([137,80,78,71,13,10,26,10]));
  run('python3', ['-c', `
import sys
sys.path.insert(0, ${JSON.stringify(path.resolve('scripts'))})
from tui_session import Session
s = Session('npm-installed', ${JSON.stringify(path.resolve('examples/night-shift'))}, env_override={'PEEP_TEST_BINARY': sys.argv[1]})
try:
    s.expect(b'peep')
    s.finish()
finally:
    if s.proc.poll() is None: s.abort()
`, executable]);
  console.log(`Offline npm installation, version, embedded Mermaid and terminal restoration passed (${process.platform}-${process.arch}).`);
} finally { fs.rmSync(tmp, {recursive:true, force:true}); }
