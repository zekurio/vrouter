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
  console.error('Usage: node scripts/import-local-auth.mjs [credential.json ...]');
}

const extraPaths = [];
for (const arg of process.argv.slice(2)) {
  if (arg === '-h' || arg === '--help') {
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

// The Codex and Claude CLIs each keep one sign-in file in the home directory.
const nativePaths = [
  path.join(os.homedir(), '.codex/auth.json'),
  path.join(os.homedir(), '.claude/.credentials.json'),
];

function isFile(file) {
  try {
    return fs.statSync(file).isFile();
  } catch {
    return false;
  }
}

let sources = extraPaths;
if (sources.length === 0) {
  sources = nativePaths.filter(isFile);
  if (sources.length === 0) {
    console.error('No Codex or Claude CLI credentials were found.');
    console.error('Looked for:');
    for (const file of nativePaths) console.error(`  ${file}`);
    console.error('Sign in with the Codex or Claude CLI first, or pass credential JSON paths explicitly.');
    console.error('Run with --help for usage.');
    process.exit(1);
  }
}

console.log(`Importing ${sources.length} credential file(s).`);
const result = spawnSync(binary, ['import', ...sources], {
  stdio: 'inherit',
});
if (result.error) {
  console.error(`Could not run bin/vrouter: ${result.error.message}`);
  process.exit(1);
}
process.exit(result.status ?? 1);
