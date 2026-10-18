# Changelog

This file lists the major changes of each release, what you must do when you upgrade, and the publicly known
vulnerabilities that the release fixes. The release page on GitHub also lists every merged pull request.
For upgrade steps in detail, read [docs/upgrading.md](docs/upgrading.md).

## Unreleased

Add a line here for each change that users can see, in the pull request that makes the change.

- Access rules can name people (`users`, email addresses) as well as groups.
  People are matched by a verified email (`accessRules.usersRequireVerifiedEmail`, default true); `ambit check --production` warns (P12) when it is false.
  Upgrade the controller and the ambit-guard together (the chart does this). An older guard does not know `users` and does not apply argument limits for people listed there.
- The console refuses an access rule that grants exactly what another rule grants.
- The access rule form suggests known group names as you type and says that groups come from your identity provider.
- The console sign-in page tells you where to find the bootstrap admin password if you lose it.
- ambitproject.dev page views reach PostHog, and the site's own fonts load; the site's Content-Security-Policy blocked both in 0.2.2.
- The changelog moved to its own menu, Releases, with one page per version and an RSS feed.
- The by-hand local install and Install on your cluster pages install 0.2.2; they still installed 0.1.0, and the quick start's go install named 0.2.0. The by-hand page is now named Install locally by hand and is under Reference.
- Connect your coding agents is now under Getting started, right after the Quick start; its first steps follow the console's Keys page, and the button name is Create key.
- The top menu no longer has Quick start; it is the first page under Docs.
- Admins can add a model in the console, with the API key typed in the console or taken from an existing Secret.
- CI: the CLI snapshot build is named after the newest release tag, not a site deploy tag.

## 0.2.2 (2026-10-17)

- The website has a changelog page, [ambitproject.dev/releases](https://ambitproject.dev/releases), made from CHANGELOG.md.
- The `ambit local up` section of the quick start is shorter and states the order in which the OpenRouter key is chosen.
- The console box "Connect a tool" (Keys page) shows the `ambit connect` command first for Claude Code, Codex, opencode and Cursor; the manual steps are shorter and come second.
- The three guides (coding tools; Codex, opencode and Hermes; other agents) are now one shorter page, "Connect your coding agents", at `guides/coding-tools`. The two old addresses redirect to it.
- ambitproject.dev counts page views with PostHog, without cookies or local storage, on the production site only.
- Maintainers: CI runs each job only when files it builds or tests change.

Upgrade: nothing to do. The charts and the API are unchanged; the console image has the new "Connect a tool" box.

## 0.2.1 (2026-10-16)

- The `ambit` command-line tool is now on Homebrew and npm for the first time: `brew install craftsman-apps/tap/ambit`
  and `npm install -g @craftsman-apps/ambit`. These two ways to install start with 0.2.1; for 0.2.0 use `install.sh`
  or `install.ps1`.
- The release builds the command-line tool for the tag that was pushed, and marks a release as the latest only when
  all of its command-line files are present.
- The Homebrew formula is written from the verified `checksums.txt` of the release.
- The npm packages are published by their absolute paths.
- Offboarding now applies while the controller waits for agentgateway to settle after a rule or mode change (see Security).
- A test of the key leader (`TestLeaderStartsTheTermAfterRepair`) failed now and then because it checked its result
  before the code under test had recorded it. The test now waits; the key leader did not change.

Upgrade: nothing to do. The chart is unchanged; the controller image has the offboarding fix.

Security: since 0.2.0, an offboarded person's sign-in token could keep working for up to about 3 seconds when an
access-rule change was being applied at the same moment; now offboarding applies at once. No security advisory was
published for this defect.

## 0.2.0 (2026-10-16)

- The `ambit` command-line tool, first released here. Install it with Homebrew (`brew install craftsman-apps/tap/ambit`),
  npm (`npx @craftsman-apps/ambit`), `install.sh` (macOS and Linux) or `install.ps1` (Windows). Commands:
  `ambit local up`, `local down` and `local password` (a local Ambit in a kind cluster), `ambit check` (including
  `--production`), `ambit validate` (checks Ambit YAML files), and `ambit version`. See [docs/cli.md](docs/cli.md).
- `ambit connect` and `ambit disconnect` set up Claude Code, Codex, opencode and Cursor to use your Ambit gateway
  with a personal key, and put the settings back. The key is kept in the keychain of your computer, or in a file
  with mode 0600, and never in a tool's config file. `ambit key print` prints the key for a tool that needs it
  (it refuses on a terminal unless you pass `--reveal`). `disconnect` does not revoke the key; revoke it in the console.
  On Windows PowerShell 5.1, `connect opencode` warns when the execution policy does not load profile scripts and
  prints the command to run. See [docs/guides/coding-tools.md](docs/guides/coding-tools.md).
- New guides: [Claude Desktop with Ambit's models](docs/guides/claude-desktop.md), [Claude Desktop with MCP tools](docs/guides/claude-desktop-mcp.md),
  [connect your coding agents](docs/guides/coding-tools.md),
  [Turn Ambit on without blocking anyone](docs/guides/audit-only.md), and the pages [install](docs/install.md),
  [CLI](docs/cli.md), [concepts](docs/concepts.md) and [compliance](docs/compliance.md).
- Known limitation: when the model router refuses a request (status 403 or 404), Ambit records the refusal but not
  who made it (an agentgateway v1.6.0 bug). See [Known limitations](docs/index.md#known-limitations).
- Audit-only mode (`access.mode: audit`): every signed-in request to a model or an MCP tool goes through, every tool is
  listed to everyone, and each audit record says whether the access rules in force at that moment would have refused
  it. The console shows a banner, a "would be refused" badge and filter, and a summary on the Audit page.
  `ambit check --production` has the new check P11. Sign-in, offboarding, budgets, argument limits and tool approval
  stay enforced. See [Turn Ambit on without blocking anyone](docs/guides/audit-only.md).
- Each change of a mode or of the access rules now waits until agentgateway reports it applied, and a change that is
  not applied within `access.activationTimeout` (120 s) shows `ModeSwitchStalled` or `RulesNotActive` on the
  AccessRules. `ambit local up` waits for the first rule to be Ready.
- `ambit connect` and `ambit disconnect` wait up to 60 seconds for the keychain (the unlock dialog of a Linux
  desktop); `ambit key print` still waits 2 seconds.
- Fix: `helm upgrade --reuse-values` from 0.1.0 works. A missing `access` value means `enforce`, an activation timeout
  of 120 s and a settle time of 3 s.

Upgrade: the AgentgatewayPolicy `ambit-access-log` is now written by `ambit-controller` (same name), so there is a
gap of about a second without an access-log policy during the upgrade, and a rollback to an earlier chart needs four
manual commands. See [docs/upgrading.md](docs/upgrading.md#to-the-release-with-audit-only-mode).

## 0.1.0 (2026-10-12)

First release. Ambit is Alpha software: the API is `ambitproject.dev/v1alpha1` and can change between releases.

- Access rules from identity-provider groups for models and for MCP servers and their tools.
- MCP servers run by ToolHive or remote, with per-person sign-in to work apps, tool pinning, argument limits and
  masking of personal data in tool results.
- Personal API keys for tools that cannot refresh a sign-in, and offboarding by SCIM that ends sessions and keys
  and revokes app grants where the vendor allows it.
- Daily budgets per group and per person, in requests or estimated dollars.
- Audit records of every call in ClickHouse, with a console page to search and export them.
- GitOps mode: console changes become pull requests, and Argo CD applies them.
- Container image with an SBOM and build provenance; Helm charts at `ghcr.io/craftsman-apps/charts`.

Upgrade: there is no earlier release to upgrade from.

Security: this release fixes no publicly known vulnerability.
