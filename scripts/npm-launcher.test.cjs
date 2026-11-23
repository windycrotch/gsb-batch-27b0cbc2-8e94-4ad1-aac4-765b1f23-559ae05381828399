'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const {spawn, spawnSync} = require('node:child_process');
function fixture(t, body) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'peep-launcher-'));
  t.after(() => fs.rmSync(dir, {recursive:true, force:true}));
  fs.copyFileSync('packaging/npm/peep.cjs', path.join(dir, 'peep.cjs'));
  const name = `@test/peep-${process.platform}-${process.arch}`;
  fs.writeFileSync(path.join(dir,'package.json'), JSON.stringify({name:'@test/peep',version:'1.2.3',optionalDependencies:{[name]:'1.2.3'}}));
  if (body) {
    const bin = path.join(dir,'node_modules',name,'bin');
    fs.mkdirSync(bin,{recursive:true});
    fs.writeFileSync(path.join(bin,'peep'), `#!${process.execPath}\n${body}`, {mode:0o755});
  }
  return path.join(dir,'peep.cjs');
}
test('passes literal arguments and child exit code', t => {
  const launcher = fixture(t, 'console.log(JSON.stringify(process.argv.slice(2))); process.exitCode=42;');
  const result = spawnSync(process.execPath,[launcher,'a b','$(not-a-shell)','--version'],{encoding:'utf8'});
  assert.equal(result.status,42);
  assert.deepEqual(JSON.parse(result.stdout),['a b','$(not-a-shell)','--version']);
});
test('missing native package gives actionable error', t => {
  const result = spawnSync(process.execPath,[fixture(t)],{encoding:'utf8'});
  assert.equal(result.status,1);
  assert.match(result.stderr,/--include=optional/);
});
test('termination waits for native cleanup', {timeout:10000}, async t => {
  const launcher = fixture(t, "process.on('SIGTERM',()=>{setTimeout(()=>{console.log('cleaned');process.exit(0)},50)}); console.log('ready'); setInterval(()=>{},1000);");
  const child = spawn(process.execPath,[launcher],{stdio:['ignore','pipe','pipe']});
  t.after(()=>child.kill('SIGKILL'));
  let output='';
  const done = new Promise((resolve,reject)=>{child.on('error',reject);child.on('exit',(code,signal)=>resolve({code,signal}));});
  child.stdout.on('data', chunk=>{output+=chunk; if(output.trim()==='ready') child.kill('SIGTERM');});
  assert.deepEqual(await done,{code:0,signal:null});
  assert.match(output,/cleaned/);
});
