// Start the local vrouter server with a private client API key.
// Only bin/vrouter is launched; the server owns the account store.
// VROUTER_API_KEY, VROUTER_ADDR, and VROUTER_ADMIN_TOKEN from
// the environment are preserved.
import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawn } from 'node:child_process';

process.umask(0o077);
const project = fileURLToPath(new URL('..', import.meta.url));
const dataDir = process.env.VROUTER_DATA_DIR
  || (process.env.XDG_STATE_HOME ? path.join(process.env.XDG_STATE_HOME, 'vrouter') : path.join(os.homedir(), '.local/state/vrouter'));
const keyFile = path.join(dataDir, 'client-key');
const legacyConfig = path.join(os.homedir(), '.local/state/vrouter/connection.json');
const binary = path.join(project, 'bin/vrouter');

if (!fs.existsSync(binary)) {
  console.error('bin/vrouter is missing. Build it first with: just build');
  process.exit(1);
}

function fail(message) {
  console.error(message);
  process.exit(1);
}
function savedKey(file) {
  let info;
  try { info = fs.lstatSync(file); }
  catch (error) { if (error.code === 'ENOENT') return ''; fail(`Cannot inspect client key at ${file}.`); }
  if (!info.isFile() || info.isSymbolicLink() || info.size > 4096)
    fail(`Client key must be a regular private file at ${file}.`);
  try {
    fs.chmodSync(file, 0o600);
    const key = fs.readFileSync(file, 'utf8').trim();
    if (!key) fail(`Saved client key is empty at ${file}. Restore it before starting vrouter.`);
    return key;
  } catch { fail(`Cannot read client key at ${file}. Restore access before starting vrouter.`); }
}

// Explicit configuration wins. Never replace an unreadable or empty saved key.
let apiKey = (process.env.VROUTER_API_KEY || '').trim();
if (apiKey) {
  console.log('Using the client API key from VROUTER_API_KEY.');
} else {
  try {
    fs.mkdirSync(dataDir, { recursive: true, mode: 0o700 });
    const info = fs.lstatSync(dataDir);
    if (!info.isDirectory() || info.isSymbolicLink()) fail('The data directory must be a real directory.');
    fs.chmodSync(dataDir, 0o700);
  } catch { fail(`Cannot prepare private data directory ${dataDir}.`); }
  apiKey = savedKey(keyFile);
  if (!apiKey) {
    let legacyKey = '';
    try {
      const legacy = JSON.parse(fs.readFileSync(legacyConfig, 'utf8'));
      if (typeof legacy.apiKey !== 'string' || !legacy.apiKey.trim())
        fail('The old connection file has no client API key. Set VROUTER_API_KEY explicitly.');
      legacyKey = legacy.apiKey.trim();
    } catch (error) {
      if (error.code !== 'ENOENT') fail('Cannot read the old connection file. Set VROUTER_API_KEY explicitly.');
    }
    apiKey = legacyKey || crypto.randomBytes(32).toString('base64url');
    try {
      fs.writeFileSync(keyFile, apiKey + '\n', { mode: 0o600, flag: 'wx' });
    } catch (error) {
      if (error.code === 'EEXIST') apiKey = savedKey(keyFile);
      else fail(`Cannot save the client key at ${keyFile}.`);
    }
  }
  console.log(`Using the client API key stored at ${keyFile}.`);
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
    VROUTER_API_KEY: apiKey,
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
