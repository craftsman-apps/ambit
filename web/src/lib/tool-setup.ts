// Setup text for each coding tool, shown once under a new key. Pure functions: the key is only ever placed in text
// the person copies, never in a URL or a log line.
export type Scope = "llm" | "mcp" | "both";
export type Shell = "bash" | "fish" | "powershell";
export type Setup = { llm: string; mcp: string; key: string; model: string };
// A block is plain text, or the same environment variables written for each shell.
export type Block = { label: string; text: string; shells?: undefined } | { label: string; shells: Record<Shell, string>; text?: undefined };
// A variant is another version of the same tool (opencode 2 and opencode 1). steps and blocks of the tab are the first one's.
export type Variant = { id: string; title: string; steps: string[]; blocks: Block[] };
// cli: the ambit connect command for the tools it supports (claude-code, codex, opencode, cursor), shown before the manual steps.
export type ToolTab = { id: string; title: string; steps: string[]; blocks: Block[]; warning?: string; cli?: { command: string; note: string }; variants?: Variant[] };

export const shellNames: Record<Shell, string> = { bash: "bash / zsh", fish: "fish", powershell: "PowerShell" };

// Cursor calls the model from its own servers, so the address must be reachable from the internet over HTTPS.
export function isPublicHttps(url: string): boolean {
  let u: URL;
  try { u = new URL(url); } catch { return false; }
  const h = u.hostname;
  if (u.protocol !== "https:" || !h.includes(".") || h.startsWith("[")) return false;
  if (/(^|\.)(localhost|localtest\.me|local|internal|lan|home\.arpa)$/.test(h)) return false;
  const ip = h.match(/^(\d+)\.(\d+)\.\d+\.\d+$/);
  if (ip) {
    const [a, b] = [Number(ip[1]), Number(ip[2])];
    return !(a === 10 || a === 127 || a === 0 || (a === 169 && b === 254) || (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 168));
  }
  return true;
}

const vars = (pairs: [string, string][]): Record<Shell, string> => ({
  bash: pairs.map(([k, v]) => `export ${k}="${v}"`).join("\n"),
  fish: pairs.map(([k, v]) => `set -gx ${k} "${v}"`).join("\n"),
  powershell: pairs.map(([k, v]) => `$env:${k} = "${v}"`).join("\n"),
});
const keepShell = "The variables last until you close the terminal. To keep them, add the same lines to your shell profile.";
const askKey = "It asks for the key (the input is hidden), shows what it will change, and asks before it writes. The key goes to your keychain, not to a config file.";
const connectCmd = (tool: string, llm: string, m: string) => ({ command: `ambit connect ${tool} --url ${llm} --model ${m}`, note: askKey });

// The key the person saved goes into the keychain with this command (the address of the key's scope).
export const keySetCommand = ({ llm, mcp }: { llm: string; mcp: string }, scope: Scope) => `ambit key set --url ${scope === "mcp" ? mcp : llm}`;

// opencode 2 sends model calls from a background service that does not see the terminal, so it reads the key from a
// file that only the person can read. opencode 1 reads {file:...} too.
const ocKeyFile = "~/.config/ambit/opencode-key";
const ocKeyRef = `{file:${ocKeyFile}}`;
const ocRestart = "opencode service restart";
const writeKeyFile = (key: string): Record<Shell, string> => ({
  bash: `mkdir -p ~/.config/ambit && touch ${ocKeyFile} && chmod 600 ${ocKeyFile} && printf '%s' '${key}' > ${ocKeyFile}`,
  fish: `mkdir -p ~/.config/ambit; and touch ${ocKeyFile}; and chmod 600 ${ocKeyFile}; and printf '%s' '${key}' > ${ocKeyFile}`,
  // the file is limited to the person before the key goes in; no key is written when icacls fails
  powershell: `New-Item -ItemType Directory -Force "$HOME\\.config\\ambit" | Out-Null; New-Item -ItemType File -Force "$HOME\\.config\\ambit\\opencode-key" | Out-Null; icacls "$HOME\\.config\\ambit\\opencode-key" /inheritance:r /grant:r "\${env:USERNAME}:F"; if ($LASTEXITCODE -eq 0) { Set-Content -NoNewline -Path "$HOME\\.config\\ambit\\opencode-key" -Value '${key}' }`,
});

export function toolTabs({ llm, mcp, key, model }: Setup, scope: Scope): ToolTab[] {
  const m = model || "<model-id>";
  const tabs: ToolTab[] = [];
  if (scope !== "mcp") {
    const claudeEnv = { env: { ANTHROPIC_BASE_URL: llm, ANTHROPIC_AUTH_TOKEN: key, ANTHROPIC_MODEL: m,
      ANTHROPIC_DEFAULT_OPUS_MODEL: m, ANTHROPIC_DEFAULT_SONNET_MODEL: m, ANTHROPIC_DEFAULT_HAIKU_MODEL: m } };
    tabs.push({ id: "claudeCode", title: "Claude Code", cli: connectCmd("claude-code", llm, m), blocks: [{ label: "Claude Code settings", text: JSON.stringify(claudeEnv, null, 2) }], steps: [
      "Merge these values into the \"env\" object of ~/.claude/settings.json (Windows: %USERPROFILE%\\.claude\\settings.json). Do not replace the file. Never put the key in a project's .claude/settings.json: it is shared.",
      "The model variables are required: Claude Code's default model names do not exist at your provider.",
      "Restart Claude Code and run /status. \"Anthropic base URL\" must show your gateway address.",
    ] });
    tabs.push({ id: "codex", title: "Codex", cli: connectCmd("codex", llm, m), blocks: [
      { label: "~/.codex/config.toml", text: `model = "${m}"\nmodel_provider = "ambit"\n\n[model_providers.ambit]\nname = "Ambit"\nbase_url = "${llm}/v1"\nenv_key = "AMBIT_API_KEY"\nwire_api = "responses"` },
      { label: "Key, in your terminal", shells: vars([["AMBIT_API_KEY", key]]) },
    ], steps: [
      "Add the settings to ~/.codex/config.toml. Put model and model_provider above the first [table] line.",
      "Set AMBIT_API_KEY in your terminal: Codex reads the key from it. " + keepShell + " Start Codex from that terminal.",
    ] });
    const opencode2: Variant = { id: "opencode2", title: "opencode 2", blocks: [
      { label: "Key file, in your terminal", shells: writeKeyFile(key) },
      { label: "~/.config/opencode/opencode.json", text: JSON.stringify({ $schema: "https://opencode.ai/config.json", model: `ambit/${m}`,
        providers: { ambit: { name: "Ambit", package: "@opencode/ai/providers/openai-compatible", settings: { baseURL: `${llm}/v1`, apiKey: ocKeyRef },
          models: { [m]: { name: m } } } } }, null, 2) },
      { label: "Restart the opencode service", text: ocRestart },
    ], steps: [
      `Write the key to ${ocKeyFile} with the command below (Windows: %USERPROFILE%\\.config\\ambit\\opencode-key). Only you can read the file. Run it again when the key changes.`,
      "Merge the provider into ~/.config/opencode/opencode.json. Keep your other settings.",
      `opencode 2 sends model calls from a background service that does not see your terminal. Restart it after each change, so that it reads the settings and the key: ${ocRestart}. If opencode shows "authorization failed", the service has no key: check the file, then restart the service.`,
    ] };
    const opencode1: Variant = { id: "opencode1", title: "opencode 1", blocks: [
      { label: "~/.config/opencode/opencode.json", text: JSON.stringify({ $schema: "https://opencode.ai/config.json", model: `ambit/${m}`,
        provider: { ambit: { npm: "@ai-sdk/openai-compatible", name: "Ambit", options: { baseURL: `${llm}/v1`, apiKey: "{env:AMBIT_API_KEY}" },
          models: { [m]: { name: m } } } } }, null, 2) },
      { label: "Key, in your terminal", shells: vars([["AMBIT_API_KEY", key]]) },
    ], steps: [
      "Merge the provider into ~/.config/opencode/opencode.json. Keep your other settings.",
      "Set AMBIT_API_KEY in your terminal: opencode reads the key from it. " + keepShell + " Start opencode from that terminal.",
    ] };
    tabs.push({ id: "opencode", title: "opencode", cli: { command: `ambit connect opencode --url ${llm} --model ${m}`,
      note: askKey.replace("The key goes to your keychain, not to a config file.", `It finds your opencode version. For opencode 2 it also writes the key to ${ocKeyFile}, readable only by you: opencode cannot run a command to read the key, and its background service does not see your terminal.`) },
      steps: opencode2.steps, blocks: opencode2.blocks, variants: [opencode2, opencode1] });
    const cursor: ToolTab = { id: "cursor", title: "Cursor", cli: { command: `ambit connect cursor --url ${llm} --model ${m}`, note: "Cursor has no config file for this: the command prints these steps with your address and writes nothing." }, blocks: [
      { label: "OpenAI API Key", text: key },
      { label: "Override OpenAI Base URL", text: `${llm}/v1` },
      { label: "Model name", text: m },
    ], steps: [
      "In Cursor, open Settings, then Models.",
      "Under API Keys, turn on \"OpenAI API Key\" and paste the key. Turn on \"Override OpenAI Base URL\" and paste the address.",
      "Add the model name with \"Add model\". Cursor has no settings file for this.",
    ] };
    if (!isPublicHttps(llm)) {
      cursor.warning = `Cursor sends its requests from Cursor's servers, not from your computer. The LLM address ${llm} is not a public HTTPS address, so Cursor cannot reach it. The address must be public HTTPS.`;
    }
    tabs.push(cursor);
    tabs.push({ id: "sdk", title: "SDK", blocks: [
      { label: "OpenAI SDK and other OpenAI-compatible tools", shells: vars([["OPENAI_BASE_URL", `${llm}/v1`], ["OPENAI_API_KEY", key]]) },
      { label: "Anthropic SDK", shells: vars([["ANTHROPIC_BASE_URL", llm], ["ANTHROPIC_API_KEY", key]]) },
    ], steps: [
      keepShell,
      `In your code, send the model name ${m}. Clients send the provider's model name, not the name of the Model object.`,
    ] });
  }
  if (scope !== "llm") {
    tabs.push({ id: "mcp", title: "MCP", blocks: [
      { label: "Claude Code", text: `claude mcp add --transport http ambit ${mcp} --header "Authorization: Bearer ${key}"` },
      { label: "Codex: ~/.codex/config.toml", text: `[mcp_servers.ambit]\nurl = "${mcp}"\nbearer_token_env_var = "AMBIT_API_KEY"` },
      { label: "Key for Codex, in your terminal", shells: vars([["AMBIT_API_KEY", key]]) },
      { label: "opencode: opencode.json", text: JSON.stringify({ mcp: { ambit: { type: "remote", url: mcp, oauth: false,
        headers: { Authorization: `Bearer ${ocKeyRef}` } } } }, null, 2) },
      { label: "Key file for opencode, in your terminal", shells: writeKeyFile(key) },
      { label: "Cursor (beta: being tested): ~/.cursor/mcp.json", text: JSON.stringify({ mcpServers: { ambit: { url: mcp, headers: { Authorization: `Bearer ${key}` } } } }, null, 2) },
    ], steps: [
      "Claude Code: run the command in a terminal.",
      "Codex: merge the block into ~/.codex/config.toml, set the variable in your terminal, and start Codex from that terminal. " + keepShell,
      `opencode: merge the block into ~/.config/opencode/opencode.json and write the key file. opencode 2 reads the key from the file because its background service does not see your terminal; restart it with ${ocRestart}. Keep "oauth": false: the gateway has no OAuth sign-in, so opencode does not need to try one.`,
      "Cursor: merge the block into ~/.cursor/mcp.json. This file holds the key, so keep it private. Cursor does not see variables set in a terminal when you start it from the dock or the Start menu.",
    ] });
  }
  return tabs;
}

// WAI-ARIA tabs: arrows move (and wrap), Home and End jump. Returns the id to select, or null for any other key.
export function nextTabId(ids: string[], current: string, key: string): string | null {
  const i = ids.indexOf(current);
  if (key === "ArrowRight") return ids[(i + 1) % ids.length];
  if (key === "ArrowLeft") return ids[(i - 1 + ids.length) % ids.length];
  if (key === "Home") return ids[0];
  if (key === "End") return ids[ids.length - 1];
  return null;
}

// The models the person's access rules allow, as the provider model IDs that clients send. /api/me/access lists Model names.
export function usableModelIds(models?: { name: string; model: string }[], access?: { models: string[] }): string[] {
  if (!models || !access) return [];
  return [...new Set(models.filter((m) => m.model && access.models.includes(m.name)).map((m) => m.model))];
}

export const FIRST_USE_TIMEOUT_MS = 5 * 60 * 1000;
// The status line under the snippets. null hides it: the console cannot tell a key's last use (available === false).
export function firstUseLine({ available, lastUsed, waitedMs }: { available?: boolean; lastUsed: string | null; waitedMs: number }):
  { kind: "waiting" | "seen" | "timeout"; text: string } | null {
  if (available === false) return null;
  if (lastUsed) {
    const at = new Date(lastUsed).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hourCycle: "h23" });
    return { kind: "seen", text: `First request seen at ${at}` };
  }
  if (waitedMs >= FIRST_USE_TIMEOUT_MS) return { kind: "timeout", text: "No request seen yet. Check the steps above, or see Audit." };
  return { kind: "waiting", text: "Waiting for the first request with this key…" };
}
