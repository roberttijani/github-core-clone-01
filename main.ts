// GitHub Core Clone Entrypoint
// Binds an ephemeral TCP port, registers with MAKEFUN_ENV_URL, and waits for trial completion.

import { createApp } from './app.ts';

const envUrl = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (!envUrl) {
  process.stderr.write('[forge-entry] Error: MAKEFUN_ENV_URL is not set.\n');
  process.exit(1);
}

const DELAY_MIN_MS = 250;
const DELAY_MAX_MS = 2000;
const PROBE_TIMEOUT_MS = 30000;

function waitDelay(retryCount: number): number {
  return Math.min(DELAY_MAX_MS, DELAY_MIN_MS * Math.pow(2, Math.min(retryCount, 8)));
}

const sleepAsync = (delay: number) => new Promise<void>((done) => setTimeout(done, delay));

async function registerPort(assignedPort: number): Promise<void> {
  let attempt = 0;
  while (true) {
    try {
      const resp = await fetch(`${envUrl}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port: assignedPort }),
        signal: AbortSignal.timeout(PROBE_TIMEOUT_MS),
      });
      if (resp.status === 200 || resp.status === 409) {
        return;
      }
      if (resp.status === 400 || resp.status === 405) {
        const text = await resp.text();
        throw new Error(`Registration failed with HTTP ${resp.status}: ${text}`);
      }
    } catch (err: unknown) {
      if (err instanceof Error && err.message.startsWith('Registration failed with HTTP ')) {
        throw err;
      }
    }
    await sleepAsync(waitDelay(attempt++));
  }
}

async function pollUntilComplete(): Promise<void> {
  let attempt = 0;
  while (true) {
    try {
      const resp = await fetch(`${envUrl}/done`, {
        signal: AbortSignal.timeout(PROBE_TIMEOUT_MS),
      });
      if (resp.ok) {
        const payload = (await resp.json()) as { done?: boolean };
        if (payload?.done === true) {
          return;
        }
      }
    } catch {
      // Retry on network timeout or transient error
    }
    await sleepAsync(waitDelay(attempt++));
  }
}

async function runEntrypoint(): Promise<void> {
  const appServer = createApp();
  await new Promise<void>((resolve, reject) => {
    appServer.once('error', reject);
    appServer.listen(0, '127.0.0.1', () => resolve());
  });

  const boundAddr = appServer.address();
  if (!boundAddr || typeof boundAddr === 'string') {
    throw new Error('Server failed to obtain an ephemeral TCP port.');
  }

  process.stdout.write(`[forge-entry] Listening on port ${boundAddr.port}\n`);
  await registerPort(boundAddr.port);
  process.stdout.write('[forge-entry] Successfully registered with env harness.\n');

  await pollUntilComplete();
  appServer.close();
  process.stdout.write('[forge-entry] Finished successfully.\n');
}

try {
  await runEntrypoint();
  process.exit(0);
} catch (error: unknown) {
  process.stderr.write(`[forge-entry] Fatal: ${error instanceof Error ? error.message : String(error)}\n`);
  process.exit(1);
}
