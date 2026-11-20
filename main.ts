// GitHub Core Clone Entrypoint
// Spawns high-performance compiled forge binary (or falls back to in-process Node core),
// registers with MAKEFUN_ENV_URL, and waits for trial completion.

import { spawn, type ChildProcess } from 'node:child_process';
import { existsSync } from 'node:fs';
import { resolve } from 'node:path';
import { createInterface } from 'node:readline';
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
  const isLinux = process.platform === 'linux';
  const linuxBin = resolve(import.meta.dirname, 'forge-linux-amd64');
  const winBin = resolve(import.meta.dirname, 'forge.exe');
  const binaryPath = isLinux ? linuxBin : (existsSync(winBin) ? winBin : null);

  let port = 0;
  let child: ChildProcess | null = null;
  let nodeServer: any = null;

  if (binaryPath && existsSync(binaryPath)) {
    process.stdout.write(`[forge-entry] Spawning native forge binary: ${binaryPath}\n`);
    if (isLinux) {
      try {
        const { chmodSync } = await import('node:fs');
        chmodSync(binaryPath, 0o755);
      } catch {}
    }

    child = spawn(binaryPath, ['--port', '0'], {
      stdio: ['ignore', 'pipe', 'inherit'],
      env: process.env,
    });

    port = await new Promise<number>((resolvePort, reject) => {
      child!.once('error', reject);
      child!.once('exit', (code) => reject(new Error(`Native binary exited early with code ${code}`)));

      const rl = createInterface({ input: child!.stdout! });
      rl.on('line', (line) => {
        const match = line.match(/^READY\s+(\d+)$/);
        if (match) {
          resolvePort(parseInt(match[1], 10));
        }
      });
    });
  } else {
    process.stdout.write('[forge-entry] Running in-process TypeScript engine...\n');
    nodeServer = createApp();
    await new Promise<void>((resolveServer, reject) => {
      nodeServer.once('error', reject);
      nodeServer.listen(0, '127.0.0.1', () => resolveServer());
    });
    const boundAddr = nodeServer.address();
    if (!boundAddr || typeof boundAddr === 'string') {
      throw new Error('Server failed to obtain an ephemeral TCP port.');
    }
    port = boundAddr.port;
  }

  process.stdout.write(`[forge-entry] Listening on port ${port}\n`);
  await registerPort(port);
  process.stdout.write('[forge-entry] Successfully registered with env harness.\n');

  await pollUntilComplete();

  if (child) {
    try {
      await fetch(`http://127.0.0.1:${port}/done`, { method: 'POST' });
    } catch {}
    child.kill('SIGTERM');
  } else if (nodeServer) {
    nodeServer.close();
  }

  process.stdout.write('[forge-entry] Finished successfully.\n');
}

try {
  await runEntrypoint();
  process.exit(0);
} catch (error: unknown) {
  process.stderr.write(`[forge-entry] Fatal: ${error instanceof Error ? error.message : String(error)}\n`);
  process.exit(1);
}
