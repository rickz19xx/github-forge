// main.ts — the entry point, and nothing else.
//
// The forge is a statically linked Go binary committed beside this file (go/ is its source). This
// script is the launcher the rules ask for: it starts that binary with this process's environment
// and its stdout/stderr attached, forwards the signals it receives, and exits with the binary's
// own code. The lifecycle the env agrees on — bind an ephemeral port, POST it to
// $MAKEFUN_ENV_URL/register, serve, poll /done, exit 0 — belongs to the binary, so there is one
// process doing the work and no hop between the client and the server.
//
// Nothing here reads a clock, a random source or the filesystem beyond locating the binary, and
// the binary writes nothing to disk: /bot is mounted read-only in the trial.

import { spawn, type ChildProcess } from 'node:child_process';
import { chmodSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

/** The directory this repository is mounted at (or checked out in). */
const ROOT = dirname(fileURLToPath(import.meta.url));

/** The committed binary: linux/amd64 in the trial, an exe for a local run on Windows. */
const BINARY = join(ROOT, process.platform === 'win32' ? 'forge.exe' : 'forge');

/** Start the binary once; `retry` allows one permission fix and a second attempt. */
function launch(retry: boolean): Promise<ChildProcess> {
  return new Promise((resolve, reject) => {
    const child = spawn(BINARY, [], { stdio: 'inherit', windowsHide: true });
    child.once('error', (error: NodeJS.ErrnoException) => {
      if (retry && (error.code === 'EACCES' || error.code === 'EPERM')) {
        // A checkout can lose the executable bit; restore it and try once more.
        try {
          chmodSync(BINARY, 0o755);
          resolve(launch(false));
          return;
        } catch {
          // Fall through: the permission could not be restored, so report the original failure.
        }
      }
      reject(error);
    });
    child.once('spawn', () => resolve(child));
  });
}

let child: ChildProcess | null = null;
try {
  child = await launch(true);
} catch (error) {
  const message = error instanceof Error ? error.message : String(error);
  process.stderr.write(`[entry] the forge binary could not start (${BINARY}): ${message}\n`);
  process.exit(1);
}

for (const signal of ['SIGINT', 'SIGTERM', 'SIGHUP'] as const) {
  process.on(signal, () => {
    child?.kill(signal);
  });
}

child.on('exit', (code, signal) => {
  if (code !== null) process.exit(code);
  // A signalled child has no exit code: report the trial as failed rather than as a clean end.
  process.stderr.write(`[entry] the forge binary ended on ${signal ?? 'an unknown signal'}\n`);
  process.exit(1);
});
