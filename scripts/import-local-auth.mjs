// Import existing local provider logins into the native vrouter account store.
// The vrouter binary parses and writes the credentials; this script only
// locates candidate files and never reads or prints credential material.
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';

const project = fileURLToPath(new URL('..', import.meta.url));
const binary = path.join(project, 'bin/vrouter');

function usage() {
  console.error('Usage: node scripts/import-local-auth.mjs [--cli] [credential.json ...]');
}

const extraPaths = [];
let useCLI = false;
for (const arg of process.argv.slice(2)) {
  if (arg === '--cli') {
    useCLI = true;
  } else if (arg === '-h' || arg === '--help') {
    usage();
    process.exit(0);
  } else if (arg.startsWith('-')) {
    console.error(`Unknown option: ${arg}`);
    usage();
    process.exit(2);
  } else {
    extraPaths.push(arg);
  }
}

if (!fs.existsSync(binary)) {
  console.error('bin/vrouter is missing. Build it first with: just build');
  process.exit(1);
}

const dataDir = process.env.VROUTER_DATA_DIR
  || (process.env.XDG_STATE_HOME ? path.join(process.env.XDG_STATE_HOME, 'vrouter') : path.join(os.homedir(), '.local/state/vrouter'));
// Files the previous CLIProxyAPI importer wrote. The engine refreshed these on
// disk, so they are preferred over the CLI originals.
const legacyAuthDir = path.join(os.homedir(), '.local/state/vrouter/auth');

function isFile(file) {
  try {
    return fs.statSync(file).isFile();
  } catch {
    return false;
  }
}

let sources = [];
if (extraPaths.length > 0 && !useCLI) {
  sources = extraPaths;
} else if (useCLI) {
  sources = [
    path.join(os.homedir(), '.codex/auth.json'),
    path.join(os.homedir(), '.claude/.credentials.json'),
  ].filter(isFile);
  if (sources.length === 0) {
    console.error('No Codex or Claude CLI credentials were found. Sign in with those CLIs first.');
    process.exit(1);
  }
} else if (fs.existsSync(legacyAuthDir)) {
  sources = fs.readdirSync(legacyAuthDir)
    .filter(name => name.endsWith('.json'))
    .sort()
    .map(name => path.join(legacyAuthDir, name))
    .filter(isFile);
}
if (sources.length === 0 && extraPaths.length === 0) {
  console.error(`No previously imported CLIProxyAPI credentials found in ${legacyAuthDir}.`);
  console.error('Run with --cli to import the signed-in Codex and Claude CLI files directly,');
  console.error('or pass credential JSON paths explicitly.');
  process.exit(1);
}
if (useCLI) sources.push(...extraPaths);

console.log(`Importing ${sources.length} credential file(s) into ${dataDir}.`);
const result = spawnSync(binary, ['import', ...sources], {
  stdio: 'inherit',
  env: { ...process.env, VROUTER_DATA_DIR: dataDir },
});
if (result.error) {
  console.error(`Could not run bin/vrouter: ${result.error.message}`);
  process.exit(1);
}
process.exit(result.status ?? 1);
