// Setup text for each coding tool, shown once under a new key. Pure functions: the key is only ever placed in text
// the person copies, never in a URL or a log line.
export type Scope = "llm" | "mcp" | "both";
export type Shell = "bash" | "fish" | "powershell";
export type Setup = { llm: string; mcp: string; key: string; model: string };
// A block is plain text, or the same environment variables written for each shell.
export type Block = { label: string; text: string; shells?: undefined } | { label: string; shells: Record<Shell, string>; text?: undefined };
export type ToolTab = { id: string; title: string; steps: string[]; blocks: Block[]; warning?: string };

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

export function toolTabs({ llm, mcp, key, model }: Setup, scope: Scope): ToolTab[] {
  const m = model || "<model-id>";
  const tabs: ToolTab[] = [];
  if (scope !== "mcp") {
    const claudeEnv = { env: { ANTHROPIC_BASE_URL: llm, ANTHROPIC_AUTH_TOKEN: key, ANTHROPIC_MODEL: m,
      ANTHROPIC_DEFAULT_OPUS_MODEL: m, ANTHROPIC_DEFAULT_SONNET_MODEL: m, ANTHROPIC_DEFAULT_HAIKU_MODEL: m } };
    tabs.push({ id: "claudeCode", title: "Claude Code", blocks: [{ label: "Claude Code settings", text: JSON.stringify(claudeEnv, null, 2) }], steps: [
      "Open ~/.claude/settings.json. On Windows it is %USERPROFILE%\\.claude\\settings.json. Create it if it does not exist.",
      "Merge these values into the \"env\" object. Do not replace the whole file.",
      "Do not put the key in a project's .claude/settings.json. That file is committed and shared with everyone who clones the project.",
      "Restart Claude Code, then run /status. The line \"Anthropic base URL\" must show your gateway address.",
      "The model variables are required: Claude Code's default model names do not exist at your provider. Claude Code may print a notice that it does not know this model. The notice is harmless.",
    ] });
    tabs.push({ id: "codex", title: "Codex (beta: being tested)", blocks: [
      { label: "~/.codex/config.toml", text: `model = "${m}"\nmodel_provider = "ambit"\n\n[model_providers.ambit]\nname = "Ambit"\nbase_url = "${llm}/v1"\nenv_key = "AMBIT_API_KEY"\nwire_api = "responses"` },
      { label: "Key, in your terminal", shells: vars([["AMBIT_API_KEY", key]]) },
    ], steps: [
      "Add the settings to ~/.codex/config.toml. Put model and model_provider above the first [table] line, and keep your other settings.",
      "The key is not written to the file. Codex reads it from the variable AMBIT_API_KEY, which you set in your terminal. " + keepShell,
      "Start Codex from that terminal.",
    ] });
    tabs.push({ id: "opencode", title: "opencode (beta: being tested)", blocks: [
      { label: "~/.config/opencode/opencode.json", text: JSON.stringify({ $schema: "https://opencode.ai/config.json", model: `ambit/${m}`,
        provider: { ambit: { npm: "@ai-sdk/openai-compatible", name: "Ambit", options: { baseURL: `${llm}/v1`, apiKey: "{env:AMBIT_API_KEY}" },
          models: { [m]: { name: m } } } } }, null, 2) },
      { label: "Key, in your terminal", shells: vars([["AMBIT_API_KEY", key]]) },
    ], steps: [
      "Merge the provider into ~/.config/opencode/opencode.json. Keep your other settings.",
      "The key is not written to the file. opencode reads it from the variable AMBIT_API_KEY, which you set in your terminal. " + keepShell,
      "Start opencode from that terminal.",
    ] });
    const cursor: ToolTab = { id: "cursor", title: "Cursor", blocks: [
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
    ], steps: ["Run this command in a terminal."] });
  }
  return tabs;
}
