// main.ts — the entry <-> env lifecycle, written out here rather than inherited from the starter.
//
// The contract with the env (env/src/wire.ts) is four steps and one guarantee:
//
//   1. MAKEFUN_ENV_URL is the env's origin. No env, nothing to serve: exit 1 straight away.
//   2. Listen on port 0 and let the kernel choose. The env owns 8080; this process never takes it.
//   3. POST /register {"port": N} says where the entry is listening. The env keeps the first register
//      it accepts; a 409 means someone else got there first, which is not a failure of this process.
//   4. Poll GET /done until it answers {"done": true}; only then close the server and exit 0, so the
//      env's export (what the scorer reads) is written after every request has been answered.
//
// Retries here are exponential and capped: a register or a poll that fails is waited out, never
// treated as fatal, because the env may still be coming up when this process starts.

import { createApp } from './app.ts';

/** The env's origin, without a trailing slash. */
const ENV_ORIGIN = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');

if (ENV_ORIGIN === '') {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is unset: there is no env to register with\n');
  process.exit(1);
}

/** One register or done request may take this long before it is abandoned and retried. */
const REQUEST_TIMEOUT_MS = 30_000;
/** The waits between retries: exponential from the first, capped at the last. */
const RETRY_FIRST_MS = 200;
const RETRY_CEILING_MS = 2_000;

function pause(ms: number): Promise<void> {
  return new Promise((resolve) => {
    setTimeout(resolve, ms);
  });
}

/** The wait after `attempt` failures: 200 ms, 400 ms, ... capped at two seconds. */
function backoff(attempt: number): number {
  return Math.min(RETRY_CEILING_MS, RETRY_FIRST_MS * 2 ** Math.min(attempt, 8));
}

/** POST our port to the env until it answers 200 (accepted) or 409 (another entry won the race). */
async function announcePort(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    let fatal: string | null = null;
    try {
      const response = await fetch(`${ENV_ORIGIN}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
      if (response.status === 200 || response.status === 409) return;
      if (response.status === 400 || response.status === 405) {
        fatal = `register answered HTTP ${response.status}: ${await response.text()}`;
      }
    } catch {
      // A refused connect or a timeout means the env is not ready: try again below.
    }
    if (fatal !== null) throw new Error(fatal);
    await pause(backoff(attempt));
  }
}

/** Poll GET /done until the env reports the trial finished; returns only on {"done": true}. */
async function awaitTrialEnd(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const response = await fetch(`${ENV_ORIGIN}/done`, { signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
      if (response.ok) {
        const body = (await response.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // Not answering yet, or the poll timed out: the trial is not over, so keep polling.
    }
    await pause(backoff(attempt));
  }
}

/** Bind an ephemeral port, hand it to the env, serve until the env says the trial is over. */
async function run(): Promise<void> {
  const server = createApp();
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, () => {
      resolve();
    });
  });
  const address = server.address();
  if (address === null || typeof address === 'string') throw new Error('the server did not bind an ephemeral TCP port');
  process.stdout.write(`[entry] listening on port ${address.port}\n`);

  await announcePort(address.port);
  process.stdout.write('[entry] registered; waiting for the env to say done\n');

  await awaitTrialEnd();
  server.close();
  process.stdout.write('[entry] done\n');
}

try {
  await run();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
