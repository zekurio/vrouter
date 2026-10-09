// Start the local vrouter server.
// Launches bin/vrouter with the current environment and forwards SIGINT
// and SIGTERM. The binary owns the data-directory and listen-address defaults.
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawn } from 'node:child_process';

const project = fileURLToPath(new URL('..', import.meta.url));
const binary = path.join(project, 'bin/vrouter');

if (!fs.existsSync(binary)) {
  console.error('bin/vrouter is missing. Build it first with: just build');
  process.exit(1);
}

let stopping = false;
const child = spawn(binary, [], {
  cwd: project,
  stdio: 'inherit',
});
for (const signal of ['SIGINT', 'SIGTERM']) {
  process.on(signal, () => {
    if (stopping) return;
    stopping = true;
    child.kill(signal);
  });
}
child.on('error', error => {
  console.error(`Could not start bin/vrouter: ${error.message}`);
  process.exitCode = 1;
});
child.on('exit', code => {
  process.exitCode = stopping ? 0 : (code ?? 1);
});
