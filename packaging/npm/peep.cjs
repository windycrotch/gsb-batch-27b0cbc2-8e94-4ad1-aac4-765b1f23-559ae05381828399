#!/usr/bin/env node
'use strict';

const { spawn } = require('node:child_process');
const path = require('node:path');
const metadata = require('./package.json');
const target = `${process.platform}-${process.arch}`;
const packageName = `${metadata.name}-${target}`;
if (!Object.hasOwn(metadata.optionalDependencies, packageName)) {
  console.error(`peep: unsupported platform ${target}; supported: macOS/Linux on x64 and arm64.`);
  process.exit(1);
}
let executable;
try {
  executable = require.resolve(`${packageName}/bin/peep`);
} catch {
  console.error(`peep: missing ${packageName}@${metadata.version}. Reinstall with npm install -g ${metadata.name} --include=optional.`);
  process.exit(1);
}
// Inherit the actual terminal, including stdin, so peep owns its TUI normally.
// Forward termination signals and wait for peep to restore the terminal.
const child = spawn(executable, process.argv.slice(2), { stdio: 'inherit' });
const handlers = new Map();
for (const signal of ['SIGINT', 'SIGTERM', 'SIGHUP']) {
  const handler = () => child.kill(signal);
  handlers.set(signal, handler);
  process.on(signal, handler);
}
const cleanup = () => {
  for (const [signal, handler] of handlers) process.removeListener(signal, handler);
};
child.on('error', error => {
  cleanup();
  console.error(`peep: cannot launch ${path.basename(executable)}: ${error.message}`);
  process.exitCode = 1;
});
child.on('exit', (code, signal) => {
  cleanup();
  if (signal) process.kill(process.pid, signal);
  else process.exitCode = code ?? 1;
});
