/**
 * Resident process entrypoint (backend dev-design §1.4). Loads config, builds the
 * Fastify app, listens, and handles graceful shutdown on SIGTERM/SIGINT: stop
 * accepting connections, drain in-flight turns, dispose sessions, exit.
 */
import { buildApp } from "./app.js";
import { loadConfig } from "./config.js";

async function main(): Promise<void> {
  const config = loadConfig();
  if (config.tokens.length === 0) {
    // Fail closed: without a token no request but /health can ever succeed.
    process.stderr.write(
      "[agent-runtime] WARNING: no AGENT_RUNTIME_TOKEN(S) configured; all authenticated routes will return 401.\n",
    );
  }
  const app = buildApp(config);

  const shutdown = async (signal: string): Promise<void> => {
    process.stderr.write(`[agent-runtime] ${signal} received, shutting down...\n`);
    try {
      await app.close();
    } finally {
      process.exit(0);
    }
  };
  process.on("SIGTERM", () => void shutdown("SIGTERM"));
  process.on("SIGINT", () => void shutdown("SIGINT"));

  await app.listen({ host: config.host, port: config.port });
  process.stderr.write(
    `[agent-runtime] listening on http://${config.host}:${config.port} (backend=${config.backend}, verboseDefault=${config.verboseDefault})\n`,
  );
}

main().catch((e) => {
  process.stderr.write(`[agent-runtime] fatal: ${e instanceof Error ? e.stack ?? e.message : String(e)}\n`);
  process.exit(1);
});
