// The per-tool setup text on the "key created" screen. Pure functions, so they run without a browser.
import { test } from "node:test";
import assert from "node:assert/strict";
import { toolTabs, isPublicHttps, nextTabId, usableModelIds, firstUseLine, FIRST_USE_TIMEOUT_MS } from "../src/lib/tool-setup.ts";

const setup = { llm: "https://llm.example.com", mcp: "https://mcp.example.com/mcp", key: "ambit_TESTKEY", model: "acme/model-1" };
const tab = (id, scope = "both") => toolTabs(setup, scope).find((t) => t.id === id);
const text = (t) => JSON.stringify(t);

test("Claude Code: names the file, merges into env, sets the model variables, says restart and /status", () => {
  const t = tab("claudeCode");
  const body = t.blocks[0].text;
  const env = JSON.parse(body).env;
  assert.equal(env.ANTHROPIC_BASE_URL, setup.llm);
  assert.equal(env.ANTHROPIC_AUTH_TOKEN, setup.key);
  for (const v of ["ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL"]) {
    assert.equal(env[v], setup.model, v);
  }
  const prose = t.steps.join("\n");
  assert.match(prose, /~\/\.claude\/settings\.json/);
  assert.match(prose, /"env"/);
  assert.match(prose, /project.*\.claude\/settings\.json/s); // the warning against the shared project file
  assert.match(prose, /Restart Claude Code/);
  assert.match(prose, /\/status/);
});

test("tabs are Claude Code, Codex, opencode, Cursor, SDK, MCP in that order", () => {
  assert.deepEqual(toolTabs(setup, "both").map((t) => t.id), ["claudeCode", "codex", "opencode", "cursor", "sdk", "mcp"]);
  assert.deepEqual(toolTabs(setup, "both").map((t) => t.title),
    ["Claude Code", "Codex (beta: being tested)", "opencode (beta: being tested)", "Cursor", "SDK", "MCP"]);
});

test("scope: llm keys get no MCP tab, mcp keys get only the MCP tab", () => {
  assert.deepEqual(toolTabs(setup, "llm").map((t) => t.id), ["claudeCode", "codex", "opencode", "cursor", "sdk"]);
  assert.deepEqual(toolTabs(setup, "mcp").map((t) => t.id), ["mcp"]);
});

test("Codex: config.toml provider with responses wire and env_key, key only in the shell variable", () => {
  const t = tab("codex");
  const toml = t.blocks[0].text;
  assert.match(t.steps.join("\n"), /~\/\.codex\/config\.toml/);
  assert.match(toml, /model_provider = "ambit"/);
  assert.match(toml, /\[model_providers\.ambit\]/);
  assert.match(toml, /base_url = "https:\/\/llm\.example\.com\/v1"/);
  assert.match(toml, /wire_api = "responses"/);
  assert.match(toml, /env_key = "AMBIT_API_KEY"/);
  assert.ok(!toml.includes(setup.key), "the key must not be written into config.toml");
  assert.match(toml, /model = "acme\/model-1"/);
  const vars = t.blocks[1].shells;
  assert.equal(vars.bash, 'export AMBIT_API_KEY="ambit_TESTKEY"');
});

test("opencode: opencode.json provider with the openai-compatible package", () => {
  const t = tab("opencode");
  const json = JSON.parse(t.blocks[0].text);
  assert.match(t.steps.join("\n"), /~\/\.config\/opencode\/opencode\.json/);
  assert.equal(json.provider.ambit.npm, "@ai-sdk/openai-compatible");
  assert.equal(json.provider.ambit.options.baseURL, "https://llm.example.com/v1");
  assert.equal(json.provider.ambit.options.apiKey, "{env:AMBIT_API_KEY}");
  assert.ok(json.provider.ambit.models["acme/model-1"]);
  assert.equal(json.model, "ambit/acme/model-1");
  assert.ok(!t.blocks[0].text.includes(setup.key));
});

test("shell variants: bash/zsh, fish and PowerShell each use their own syntax", () => {
  const sdk = tab("sdk");
  const shells = sdk.blocks.find((b) => b.shells).shells;
  assert.match(shells.bash, /^export OPENAI_BASE_URL="https:\/\/llm\.example\.com\/v1"\nexport OPENAI_API_KEY="ambit_TESTKEY"$/);
  assert.match(shells.fish, /^set -gx OPENAI_BASE_URL "https:\/\/llm\.example\.com\/v1"\nset -gx OPENAI_API_KEY "ambit_TESTKEY"$/);
  assert.match(shells.powershell, /^\$env:OPENAI_BASE_URL = "https:\/\/llm\.example\.com\/v1"\n\$env:OPENAI_API_KEY = "ambit_TESTKEY"$/);
  const anth = sdk.blocks.filter((b) => b.shells)[1].shells;
  assert.match(anth.bash, /export ANTHROPIC_BASE_URL="https:\/\/llm\.example\.com"\nexport ANTHROPIC_API_KEY=/);
  assert.match(anth.powershell, /\$env:ANTHROPIC_BASE_URL = /);
});

test("Cursor: three UI settings, no file", () => {
  const t = tab("cursor");
  const all = text(t);
  assert.match(all, /OpenAI API Key/);
  assert.match(all, /Override OpenAI Base URL/);
  assert.match(all, /https:\/\/llm\.example\.com\/v1/);
  assert.equal(t.warning, undefined);
});

test("Cursor: warns when the LLM address is not public HTTPS", () => {
  for (const llm of ["http://llm.localtest.me:8080", "http://llm.example.com", "https://localhost:8443", "https://10.0.0.5", "https://llm.internal", "not a url"]) {
    const t = toolTabs({ ...setup, llm }, "both").find((x) => x.id === "cursor");
    assert.match(t.warning ?? "", /Cursor/, llm);
    assert.match(t.warning, /public HTTPS/, llm);
  }
});

test("isPublicHttps", () => {
  assert.equal(isPublicHttps("https://llm.example.com"), true);
  assert.equal(isPublicHttps("https://llm.example.com:8443/x"), true);
  for (const u of ["http://llm.example.com", "https://localhost", "https://127.0.0.1", "https://192.168.1.4", "https://172.20.0.1",
    "https://foo.localtest.me", "https://[::1]", "https://nodot", "", "x"]) assert.equal(isPublicHttps(u), false, u);
});

test("MCP: claude mcp add with the header", () => {
  const t = tab("mcp");
  assert.equal(t.blocks[0].text, 'claude mcp add --transport http ambit https://mcp.example.com/mcp --header "Authorization: Bearer ambit_TESTKEY"');
});

test("no tab puts the key in a URL", () => {
  for (const t of toolTabs(setup, "both")) {
    for (const b of t.blocks) for (const s of b.shells ? Object.values(b.shells) : [b.text]) {
      assert.ok(!/https?:\/\/[^\s"']*ambit_TESTKEY/.test(s), `${t.id}: key in URL`);
    }
  }
});

test("without a model the snippets show a visible placeholder", () => {
  const t = toolTabs({ ...setup, model: "" }, "both").find((x) => x.id === "claudeCode");
  assert.equal(JSON.parse(t.blocks[0].text).env.ANTHROPIC_MODEL, "<model-id>");
});

test("MCP tab: Claude Code, Codex, opencode and Cursor, with a static header and no OAuth", () => {
  const t = tab("mcp");
  const by = (re) => t.blocks.find((b) => re.test(b.label));
  const codex = by(/Codex/);
  assert.match(codex.label, /beta/);
  assert.match(codex.text, /\[mcp_servers\.ambit\]/);
  assert.match(codex.text, /url = "https:\/\/mcp\.example\.com\/mcp"/);
  assert.match(codex.text, /bearer_token_env_var = "AMBIT_API_KEY"/);
  assert.ok(!codex.text.includes(setup.key));
  const oc = by(/opencode/);
  assert.match(oc.label, /beta/);
  const mcp = JSON.parse(oc.text).mcp.ambit;
  assert.deepEqual([mcp.type, mcp.url, mcp.oauth], ["remote", setup.mcp, false]);
  assert.equal(mcp.headers.Authorization, "Bearer {env:AMBIT_API_KEY}");
  const cur = by(/Cursor/);
  assert.match(cur.label, /beta/);
  const c = JSON.parse(cur.text).mcpServers.ambit;
  assert.equal(c.url, setup.mcp);
  assert.equal(c.headers.Authorization, "Bearer ambit_TESTKEY");
  assert.match(cur.label, /\.cursor\/mcp\.json/);
  assert.ok(t.blocks.some((b) => b.shells && b.shells.bash.includes("AMBIT_API_KEY")), "a shell block sets AMBIT_API_KEY");
});

test("tab keys: arrows wrap, Home and End jump, other keys do nothing", () => {
  const ids = ["a", "b", "c"];
  assert.equal(nextTabId(ids, "a", "ArrowRight"), "b");
  assert.equal(nextTabId(ids, "c", "ArrowRight"), "a");
  assert.equal(nextTabId(ids, "a", "ArrowLeft"), "c");
  assert.equal(nextTabId(ids, "b", "Home"), "a");
  assert.equal(nextTabId(ids, "b", "End"), "c");
  assert.equal(nextTabId(ids, "b", "x"), null);
});

test("model list: only the models the person may use, as provider model IDs", () => {
  const models = [{ name: "free", model: "nvidia/x:free" }, { name: "sonnet", model: "anthropic.sonnet" }, { name: "dup", model: "nvidia/x:free" }];
  assert.deepEqual(usableModelIds(models, { models: ["dup", "free"] }), ["nvidia/x:free"]);
  assert.deepEqual(usableModelIds(models, { models: [] }), []);
  assert.deepEqual(usableModelIds(undefined, undefined), []);
});

test("first-use line: hidden, waiting, seen, timeout", () => {
  const at = new Date(Date.UTC(2026, 9, 14, 12, 3)).toISOString();
  assert.equal(firstUseLine({ available: false, lastUsed: null, waitedMs: 0 }), null);
  assert.equal(firstUseLine({ available: false, lastUsed: at, waitedMs: 0 }), null);
  assert.deepEqual(firstUseLine({ available: true, lastUsed: null, waitedMs: 1000 }), { kind: "waiting", text: "Waiting for the first request with this key…" });
  assert.match(firstUseLine({ available: true, lastUsed: at, waitedMs: 1000 }).text, /^First request seen at \d\d:\d\d$/);
  assert.equal(firstUseLine({ available: true, lastUsed: at, waitedMs: 1000 }).kind, "seen");
  assert.deepEqual(firstUseLine({ available: true, lastUsed: null, waitedMs: FIRST_USE_TIMEOUT_MS }),
    { kind: "timeout", text: "No request seen yet. Check the steps above, or see Audit." });
  assert.equal(FIRST_USE_TIMEOUT_MS, 5 * 60 * 1000);
  // a request that arrives after the timeout still wins
  assert.equal(firstUseLine({ available: true, lastUsed: at, waitedMs: FIRST_USE_TIMEOUT_MS * 2 }).kind, "seen");
  // while the first answer is still loading, show waiting
  assert.equal(firstUseLine({ available: undefined, lastUsed: null, waitedMs: 0 }).kind, "waiting");
});
