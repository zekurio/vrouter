// Start the local vrouter server.
// Launches bin/vrouter with the current environment and forwards SIGINT
// and SIGTERM.
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawn } from 'node:child_process';

const project = fileURLToPath(new URL('..', import.meta.url));
const binary = path.join(project, 'bin/vrouter');
const dataDir = process.env.VROUTER_DATA_DIR
  || (process.env.XDG_STATE_HOME ? path.join(process.env.XDG_STATE_HOME, 'vrouter') : path.join(os.homedir(), '.local/state/vrouter'));

if (!fs.existsSync(binary)) {
  console.error('bin/vrouter is missing. Build it first with: just build');
  process.exit(1);
}

const children = new Set();
let stopping = false;
function stop(signal) {
  if (stopping) return;
  stopping = true;
  for (const child of children) child.kill(signal);
}
for (const signal of ['SIGINT', 'SIGTERM']) {
  process.on(signal, () => stop(signal));
}

const child = spawn(binary, [], {
  cwd: project,
  stdio: 'inherit',
  env: {
    ...process.env,
    VROUTER_DATA_DIR: dataDir,
    VROUTER_ADDR: process.env.VROUTER_ADDR || '127.0.0.1:8080',
  },
});
children.add(child);
child.on('error', error => {
  console.error(`Could not start bin/vrouter: ${error.message}`);
  process.exitCode = 1;
});
child.on('exit', code => {
  children.delete(child);
  process.exitCode = stopping ? 0 : (code ?? 1);
});
