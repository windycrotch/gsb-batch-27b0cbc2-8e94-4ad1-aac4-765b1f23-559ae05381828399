'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const {spawnSync} = require('node:child_process');
function fixture(t, mode) {
  const dir=fs.mkdtempSync(path.join(os.tmpdir(),'peep-publish-'));
  t.after(()=>fs.rmSync(dir,{recursive:true,force:true}));
  const root=path.join(dir,'assets'), bin=path.join(dir,'bin'); fs.mkdirSync(root);fs.mkdirSync(bin);
  const dependencies=Object.fromEntries(['darwin-arm64','darwin-x64','linux-arm64','linux-x64'].map(p=>['@test/peep-'+p,'1.2.3']));
  for(const name of [...Object.keys(dependencies),'@test/peep']){
    const pkg=path.join(dir,'package');fs.mkdirSync(pkg,{recursive:true});
    fs.writeFileSync(path.join(pkg,'package.json'),JSON.stringify({name,version:'1.2.3',...(name==='@test/peep'?{bin:{peep:'peep.cjs'},optionalDependencies:dependencies}:{})}));
    const result=spawnSync('tar',['-czf',path.join(root,name.replace('@','').replace('/','-')+'.tgz'),'-C',dir,'package']);assert.equal(result.status,0);
  }
  const npm=path.join(bin,'npm');
  fs.writeFileSync(npm,`#!${process.execPath}
const fs=require('node:fs'),path=require('node:path'),crypto=require('node:crypto');
const [command,spec]=process.argv.slice(2);
if(command==='view'){
 if(process.env.MODE==='missing'){console.log(JSON.stringify({error:{code:'E404'}}));process.exit(1)}
 if(process.env.MODE==='unauthorized'){console.log(JSON.stringify({error:{code:'E401'}}));process.exit(1)}
 const name=spec.slice(0,spec.lastIndexOf('@'));
 const b=fs.readFileSync(path.join(process.env.ASSETS,name.replace('@','').replace('/','-')+'.tgz'));
 console.log(JSON.stringify(process.env.MODE==='mismatch'?'sha512-wrong':'sha512-'+crypto.createHash('sha512').update(b).digest('base64')));
}else{fs.appendFileSync(process.env.LOG,fs.readFileSync(spec).length+' '+path.basename(spec)+'\\n')}
`,{mode:0o755});
  const log=path.join(dir,'published');
  const result=spawnSync(process.execPath,['scripts/npm-publish.cjs',root,'1.2.3'],{encoding:'utf8',env:{...process.env,PATH:bin+path.delimiter+process.env.PATH,MODE:mode,ASSETS:root,LOG:log}});
  return {result,published:fs.existsSync(log)?fs.readFileSync(log,'utf8'):''};
}
test('first publication installs all native packages before launcher',t=>{
 const {result,published}=fixture(t,'missing');assert.equal(result.status,0,result.stderr);const lines=published.trim().split('\n');assert.equal(lines.length,5);assert.match(lines.at(-1),/test-peep.tgz$/);
});
test('retry skips only packages with matching registry integrity',t=>{
 const {result,published}=fixture(t,'same');assert.equal(result.status,0,result.stderr);assert.equal(published,'');assert.equal(result.stdout.split('skipping').length-1,5);
});
for(const mode of ['mismatch','unauthorized'])test(`${mode} blocks publication`,t=>{
 const {result,published}=fixture(t,mode);assert.notEqual(result.status,0);assert.equal(published,'');
});
