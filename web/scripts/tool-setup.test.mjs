// The per-tool setup text on the "key created" screen. Pure functions, so they run without a browser.
import { test } from "node:test";
import assert from "node:assert/strict";
import { toolTabs, isPublicHttps, nextTabId, usableModelIds, firstUseLine, keySetCommand, plainHttpRemote, FIRST_USE_TIMEOUT_MS } from "../src/lib/tool-setup.ts";

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

test("tabs are Claude Code, Codex, opencode, Hermes Agent, Kilo Code, pi, Cursor, SDK, MCP in that order", () => {
  assert.deepEqual(toolTabs(setup, "both").map((t) => t.id), ["claudeCode", "codex", "opencode", "hermes", "kilocode", "pi", "cursor", "sdk", "mcp"]);
  assert.deepEqual(toolTabs(setup, "both").map((t) => t.title),
    ["Claude Code", "Codex", "opencode", "Hermes Agent", "Kilo Code", "pi", "Cursor", "SDK", "MCP"]);
});

test("scope: llm keys get no MCP tab, mcp keys get only the MCP tab", () => {
  assert.deepEqual(toolTabs(setup, "llm").map((t) => t.id), ["claudeCode", "codex", "opencode", "hermes", "kilocode", "pi", "cursor", "sdk"]);
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

test("opencode: opencode 2 first, with the key in a user-only file and the service restart", () => {
  const t = tab("opencode");
  assert.deepEqual(t.variants.map((v) => [v.id, v.title]), [["opencode2", "opencode 2"], ["opencode1", "opencode 1"]]);
  assert.deepEqual([t.steps, t.blocks], [t.variants[0].steps, t.variants[0].blocks]); // opencode 2 is what the tab shows first
  const prose = t.steps.join("\n");
  assert.match(prose, /~\/\.config\/opencode\/opencode\.json/);
  assert.match(prose, /~\/\.config\/ambit\/opencode-key/);
  assert.match(prose, /background service/);
  assert.match(prose, /opencode service restart/);
  assert.match(prose, /authorization failed/);
  const keyFile = t.blocks.find((b) => b.shells);
  assert.equal(keyFile.shells.bash, "mkdir -p ~/.config/ambit && touch ~/.config/ambit/opencode-key && chmod 600 ~/.config/ambit/opencode-key && printf '%s' 'ambit_TESTKEY' > ~/.config/ambit/opencode-key");
  assert.equal(keyFile.shells.fish, "mkdir -p ~/.config/ambit; and touch ~/.config/ambit/opencode-key; and chmod 600 ~/.config/ambit/opencode-key; and printf '%s' 'ambit_TESTKEY' > ~/.config/ambit/opencode-key");
  assert.match(keyFile.shells.powershell, /Set-Content -NoNewline -Path "\$HOME\\\.config\\ambit\\opencode-key" -Value 'ambit_TESTKEY'/);
  assert.match(keyFile.shells.powershell, /icacls .*\/inheritance:r \/grant:r "\$\{env:USERNAME\}:F"/);
  // the ACL is set on the empty file first, and the key is written only when icacls succeeded
  const ps = keyFile.shells.powershell;
  assert.ok(ps.indexOf("icacls") < ps.indexOf("ambit_TESTKEY") && ps.includes("if ($LASTEXITCODE -eq 0) { Set-Content"), ps);
  const json = JSON.parse(t.blocks.find((b) => /opencode\.json/.test(b.label)).text);
  assert.equal(json.provider, undefined);
  assert.deepEqual(json.providers.ambit, { name: "Ambit", package: "@opencode/ai/providers/openai-compatible",
    settings: { baseURL: "https://llm.example.com/v1", apiKey: "{file:~/.config/ambit/opencode-key}" }, models: { "acme/model-1": { name: "acme/model-1" } } });
  assert.equal(json.model, "ambit/acme/model-1");
  assert.ok(t.blocks.some((b) => b.text === "opencode service restart"));
  assert.ok(!JSON.stringify(json).includes(setup.key) && !text(t.variants[0]).includes("AMBIT_API_KEY"));
});

test("opencode 1: opencode.json provider with the openai-compatible package and the key in the terminal", () => {
  const t = tab("opencode").variants[1];
  const json = JSON.parse(t.blocks[0].text);
  assert.match(t.steps.join("\n"), /~\/\.config\/opencode\/opencode\.json/);
  assert.equal(json.provider.ambit.npm, "@ai-sdk/openai-compatible");
  assert.equal(json.provider.ambit.options.baseURL, "https://llm.example.com/v1");
  assert.equal(json.provider.ambit.options.apiKey, "{env:AMBIT_API_KEY}");
  assert.ok(json.provider.ambit.models["acme/model-1"]);
  assert.equal(json.model, "ambit/acme/model-1");
  assert.ok(!t.blocks[0].text.includes(setup.key));
  assert.equal(t.blocks[1].shells.bash, 'export AMBIT_API_KEY="ambit_TESTKEY"');
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
  assert.doesNotMatch(codex.label, /beta/);
  assert.match(codex.text, /\[mcp_servers\.ambit\]/);
  assert.match(codex.text, /url = "https:\/\/mcp\.example\.com\/mcp"/);
  assert.match(codex.text, /bearer_token_env_var = "AMBIT_API_KEY"/);
  assert.ok(!codex.text.includes(setup.key));
  const oc = by(/opencode/);
  assert.doesNotMatch(oc.label, /beta/);
  const mcp = JSON.parse(oc.text).mcp.ambit;
  assert.deepEqual([mcp.type, mcp.url, mcp.oauth], ["remote", setup.mcp, false]);
  // opencode 2's background service does not see the terminal: the header reads the key file (opencode 1 reads it too)
  assert.equal(mcp.headers.Authorization, "Bearer {file:~/.config/ambit/opencode-key}");
  assert.ok(t.blocks.some((b) => /opencode/.test(b.label) && b.shells && b.shells.bash.includes("~/.config/ambit/opencode-key")), "a block writes the key file");
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

test("Hermes Agent: key_cmd runs ambit key print, the key is stored with ambit key set, no file holds it", () => {
  const t = tab("hermes");
  assert.equal(t.blocks[0].text, "ambit key set --url https://llm.example.com");
  const yaml = t.blocks[1].text;
  assert.match(t.blocks[1].label, /~\/\.hermes\/config\.yaml/);
  assert.match(yaml, /^model:\n {2}provider: custom:ambit\n {2}default: acme\/model-1\n/);
  assert.match(yaml, /\nproviders:\n {2}ambit:\n {4}base_url: https:\/\/llm\.example\.com\/v1\n {4}api_mode: chat_completions\n {4}key_cmd: ambit key print --url https:\/\/llm\.example\.com$/);
  assert.ok(!text(t).includes(setup.key), "no Hermes block holds the key");
  const prose = t.steps.join("\n");
  assert.match(prose, /%LOCALAPPDATA%\\hermes\\config\.yaml/);
  assert.match(prose, /hermes gateway restart/);
  assert.match(prose, /PATH/);
});

test("Kilo Code: the key in a user-only file that kilo.jsonc refers to, and the restart", () => {
  const t = tab("kilocode");
  assert.equal(t.blocks[0].shells.bash, "mkdir -p ~/.config/ambit && touch ~/.config/ambit/kilo-key && chmod 600 ~/.config/ambit/kilo-key && printf '%s' 'ambit_TESTKEY' > ~/.config/ambit/kilo-key");
  assert.match(t.blocks[0].shells.powershell, /icacls "\$HOME\\\.config\\ambit\\kilo-key" \/inheritance:r .*if \(\$LASTEXITCODE -eq 0\) \{ Set-Content -NoNewline -Path "\$HOME\\\.config\\ambit\\kilo-key" -Value 'ambit_TESTKEY' \}$/);
  const conf = JSON.parse(t.blocks[1].text);
  assert.equal(conf.model, "ambit/acme/model-1");
  assert.deepEqual(conf.provider.ambit.options, { baseURL: "https://llm.example.com/v1", apiKey: "{file:~/.config/ambit/kilo-key}" });
  assert.equal(conf.provider.ambit.npm, "@ai-sdk/openai-compatible");
  assert.ok(!t.blocks[1].text.includes(setup.key));
  assert.match(t.steps.join("\n"), /Reload Window/);
  assert.match(t.cli.note, /kilo-key, readable only by you/);
});

test("pi: apiKey runs ambit key print, settings.json sets the default", () => {
  const t = tab("pi");
  assert.equal(t.blocks[0].text, "ambit key set --url https://llm.example.com");
  const p = JSON.parse(t.blocks[1].text).providers.ambit;
  assert.deepEqual(p, { baseUrl: "https://llm.example.com/v1", api: "openai-completions", apiKey: "!ambit key print --url https://llm.example.com", models: [{ id: "acme/model-1" }] });
  assert.deepEqual(JSON.parse(t.blocks[2].text), { defaultProvider: "ambit", defaultModel: "acme/model-1" });
  assert.ok(!text(t).includes(setup.key));
});

test("MCP tab: Hermes (mcp-remote and a header file), Kilo Code (key file) and pi (the provider's key)", () => {
  const t = tab("mcp");
  const by = (re) => t.blocks.find((b) => re.test(b.label));
  const h = by(/^Hermes Agent/).text;
  assert.equal(h, 'mcp_servers:\n  ambit:\n    command: npx\n    args: ["-y", "mcp-remote@0.14.3", "https://mcp.example.com/mcp", "--header-file", "${userHome}${/}.ambit${/}hermes-mcp-headers.txt"]');
  assert.equal(by(/Header file for Hermes/).shells.bash,
    "mkdir -p ~/.ambit && touch ~/.ambit/hermes-mcp-headers.txt && chmod 600 ~/.ambit/hermes-mcp-headers.txt && printf 'Authorization: Bearer %s\\n' 'ambit_TESTKEY' > ~/.ambit/hermes-mcp-headers.txt");
  assert.match(by(/Header file for Hermes/).shells.powershell, /-Value 'Authorization: Bearer ambit_TESTKEY' \}$/);
  const k = JSON.parse(by(/^Kilo Code/).text).mcp.ambit;
  assert.deepEqual(k, { type: "remote", url: setup.mcp, oauth: false, headers: { Authorization: "Bearer {file:~/.config/ambit/kilo-key}" } });
  assert.deepEqual(JSON.parse(by(/^pi:/).text), { mcpServers: { ambit: { url: setup.mcp, auth: { provider: "ambit" } } } });
  // a local install: plain http to a host that is not localhost
  const local = toolTabs({ ...setup, mcp: "http://mcp.localtest.me:8080/mcp" }, "both");
  assert.match(local.find((x) => x.id === "mcp").blocks.find((b) => /^Hermes/.test(b.label)).text, /"http:\/\/mcp\.localtest\.me:8080\/mcp", "--allow-http"/);
  assert.doesNotMatch(local.find((x) => x.id === "pi").cli.command, /--mcp-url/); // pi refuses it
  assert.match(local.find((x) => x.id === "kilocode").cli.command, /--mcp-url http:\/\/mcp\.localtest\.me:8080\/mcp$/);
  assert.equal(plainHttpRemote("http://localhost:8080/mcp"), false);
  assert.equal(plainHttpRemote("https://mcp.example.com/mcp"), false);
});

test("ambit connect: hermes, kilocode and pi add --mcp-url when the key's scope has MCP", () => {
  for (const name of ["hermes", "kilocode", "pi"]) {
    assert.equal(tab(name, "both").cli.command, `ambit connect ${name} --url https://llm.example.com --model acme/model-1 --mcp-url https://mcp.example.com/mcp`);
    assert.equal(tab(name, "llm").cli.command, `ambit connect ${name} --url https://llm.example.com --model acme/model-1`);
  }
});

test("ambit connect: shown for Claude Code, Codex, opencode and Cursor with the real command shape; not for SDK or MCP", () => {
  const tools = { claudeCode: "claude-code", codex: "codex", opencode: "opencode", cursor: "cursor" };
  for (const [id, name] of Object.entries(tools)) {
    assert.equal(tab(id).cli.command, `ambit connect ${name} --url https://llm.example.com --model acme/model-1`, id);
  }
  assert.equal(tab("sdk").cli, undefined);
  assert.equal(tab("mcp").cli, undefined);
});

test("ambit connect: the key is never in the command, and the manual steps stay at 3 or fewer for the CLI tools", () => {
  for (const id of ["claudeCode", "codex", "opencode", "hermes", "kilocode", "pi", "cursor"]) {
    assert.ok(!tab(id).cli.command.includes(setup.key), id);
    assert.ok(tab(id).steps.length <= 3, `${id}: ${tab(id).steps.length} steps`);
  }
  assert.match(tab("claudeCode").cli.note, /asks for the key/);
  assert.match(tab("cursor").cli.note, /writes nothing/);
});

test("ambit connect: without a model the command shows the placeholder", () => {
  const t = toolTabs({ ...setup, model: "" }, "llm").find((x) => x.id === "claudeCode");
  assert.match(t.cli.command, /--model <model-id>$/);
});

test("a key that is no longer shown: every tab uses the placeholder and never a real key", () => {
  const placeholder = { ...setup, key: "ambit_..." };
  for (const t of toolTabs(placeholder, "both")) {
    for (const v of t.variants ?? [t]) {
      for (const b of v.blocks) for (const s of b.shells ? Object.values(b.shells) : [b.text]) assert.ok(!s.includes("ambit_TESTKEY"), t.id);
    }
  }
  assert.match(text(toolTabs(placeholder, "both").find((t) => t.id === "opencode")), /ambit_\.\.\./);
  assert.match(text(toolTabs(placeholder, "both").find((t) => t.id === "kilocode")), /'ambit_\.\.\.' > ~\/\.config\/ambit\/kilo-key/);
});

test("ambit key set: the address of the key's scope", () => {
  assert.equal(keySetCommand(setup, "llm"), "ambit key set --url https://llm.example.com");
  assert.equal(keySetCommand(setup, "both"), "ambit key set --url https://llm.example.com");
  assert.equal(keySetCommand(setup, "mcp"), "ambit key set --url https://mcp.example.com/mcp");
});

test("service account key: environment variables and GitHub Actions; the workflow never holds the key", async () => {
  const { ciTabs } = await import("../src/lib/tool-setup.ts");
  const llm = ciTabs(setup, "llm");
  assert.deepEqual(llm.map((t) => t.id), ["env", "github"]);
  assert.match(llm[0].blocks[0].shells.bash, /export OPENAI_BASE_URL="https:\/\/llm.example.com\/v1"/);
  assert.match(llm[0].blocks[0].shells.bash, /export ANTHROPIC_API_KEY="ambit_TESTKEY"/);
  assert.doesNotMatch(text(llm[0]), /AMBIT_MCP_URL/);
  const wf = llm[1].blocks[0].text;
  assert.match(wf, /^env:\n {2}OPENAI_BASE_URL: "https:\/\/llm.example.com\/v1"\n {2}OPENAI_API_KEY: \$\{\{ secrets.AMBIT_API_KEY \}\}/);
  assert.doesNotMatch(wf, /ambit_TESTKEY/);
  assert.match(llm[1].steps.join("\n"), /New repository secret\. Name: AMBIT_API_KEY/);
  assert.match(llm[1].steps.join("\n"), /anyone who can change a workflow can read the secret/);
  const mcp = ciTabs(setup, "mcp");
  assert.doesNotMatch(text(mcp), /OPENAI_/);
  assert.match(mcp[1].blocks[0].text, /AMBIT_MCP_URL: "https:\/\/mcp.example.com\/mcp"\n {2}AMBIT_API_KEY: \$\{\{ secrets.AMBIT_API_KEY \}\}/);
});
