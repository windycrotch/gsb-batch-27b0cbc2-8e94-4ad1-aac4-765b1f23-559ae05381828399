#!/usr/bin/env node
'use strict';
const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const {spawnSync} = require('node:child_process');
const root = path.resolve(process.argv[2] || '');
const version = process.argv[3];
if (!/^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/.test(version || '')) throw Error('stable version required');
const packages = fs.readdirSync(root).filter(f=>f.endsWith('.tgz')).map(file=> {
  const archive = path.join(root,file);
  const result = spawnSync('tar',['-xOf',archive,'package/package.json'],{encoding:'utf8'});
  if (result.status !== 0) throw Error(`Cannot read ${file}: ${result.stderr}`);
  const manifest = JSON.parse(result.stdout);
  if (manifest.version !== version) throw Error(`Version mismatch in ${file}`);
  return {archive,manifest,integrity:'sha512-'+crypto.createHash('sha512').update(fs.readFileSync(archive)).digest('base64')};
});
const wrapper = packages.find(p=>p.manifest.bin?.peep);
if (!wrapper || packages.length !== 5) throw Error('Expected one launcher and four native packages');
for (const [name,v] of Object.entries(wrapper.manifest.optionalDependencies)) {
  if (v!==version || !packages.some(p=>p.manifest.name===name)) throw Error(`Missing matching ${name}`);
}
packages.sort((a,b)=>Number(!!a.manifest.bin)-Number(!!b.manifest.bin));
for (const p of packages) {
  const spec = `${p.manifest.name}@${version}`;
  const lookup = spawnSync('npm',['view',spec,'dist.integrity','--json'],{encoding:'utf8'});
  if (lookup.error) throw lookup.error;
  if (lookup.status===0) {
    if (JSON.parse(lookup.stdout)!==p.integrity) throw Error(`${spec} already exists with different bytes; choose a new version`);
    console.log(`${spec} already published with matching integrity; skipping`);
    continue;
  }
  let error; try { error=JSON.parse(lookup.stdout).error; } catch {}
  if (error?.code!=='E404') throw Error(`Cannot check ${spec}: ${lookup.stderr}`);
  const result = spawnSync('npm',['publish',p.archive,'--access','public'],{stdio:'inherit'});
  if (result.status!==0) throw Error(`Publication failed for ${spec}: ${result.error || result.status}`);
}
