// Drives `agentium acp` with the official ACP TypeScript SDK and checks
// every response and notification against the SDK's zod schemas.
// Usage: node acp_client.mjs <sdk dir> <agentium binary> <cwd>
import { spawn } from "node:child_process";
import { Readable, Writable } from "node:stream";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [sdkDir, bin, cwd] = process.argv.slice(2);
const acp = await import(pathToFileURL(path.join(sdkDir, "node_modules/@agentclientprotocol/sdk/dist/acp.js")));
const z = await import(pathToFileURL(path.join(sdkDir, "node_modules/@agentclientprotocol/sdk/dist/schema/zod.gen.js")));

const child = spawn(bin, ["acp", "-m", "fakeant/m"], { cwd, stdio: ["pipe", "pipe", "inherit"] });
const stream = acp.ndJsonStream(Writable.toWeb(child.stdin), Readable.toWeb(child.stdout));
const updates = [];
let permissions = 0;
const fail = (what, err) => { console.error("SCHEMA FAIL:", what, JSON.stringify(err.issues ?? err)); process.exit(1); };
const check = (schema, value, what) => { const r = schema.safeParse(value); if (!r.success) fail(what, r.error); };

const conn = new acp.ClientSideConnection(() => ({
  async sessionUpdate(n) { check(z.zSessionNotification, n, "session/update"); updates.push(n.update.sessionUpdate); },
  async requestPermission(p) {
    check(z.zRequestPermissionRequest, p, "request_permission");
    permissions++;
    return { outcome: { outcome: "selected", optionId: p.options.find((o) => o.kind === "allow_once").optionId } };
  },
  async readTextFile() { throw new Error("not used"); },
  async writeTextFile() { throw new Error("not used"); },
}), stream);

const init = await conn.initialize({ protocolVersion: acp.PROTOCOL_VERSION, clientCapabilities: { fs: { readTextFile: false, writeTextFile: false } } });
check(z.zInitializeResponse, init, "initialize response");
const sess = await conn.newSession({ cwd, mcpServers: [] });
check(z.zNewSessionResponse, sess, "session/new response");
await conn.setSessionMode({ sessionId: sess.sessionId, modeId: "ask" });
const res = await conn.prompt({ sessionId: sess.sessionId, prompt: [{ type: "text", text: "create hello.txt" }] });
check(z.zPromptResponse, res, "session/prompt response");
console.log(JSON.stringify({ protocolVersion: init.protocolVersion, stopReason: res.stopReason, permissions, updates }));
child.kill();
process.exit(0);
