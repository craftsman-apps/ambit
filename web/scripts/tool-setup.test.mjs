// The per-tool setup text on the "key created" screen. Pure functions, so they run without a browser.
import { test } from "node:test";
import assert from "node:assert/strict";
import { toolTabs, isPublicHttps } from "../src/lib/tool-setup.ts";

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
