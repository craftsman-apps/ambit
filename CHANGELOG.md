# Changelog

This file lists the major changes of each release, what you must do when you upgrade, and the publicly known
vulnerabilities that the release fixes. The release page on GitHub also lists every merged pull request.
For upgrade steps in detail, read [docs/upgrading.md](docs/upgrading.md).

## Unreleased

Each pull request adds a file in changelog/unreleased/ (see its README); the release assembles them here.

## 0.3.0 (2026-11-09)

### What is new

- **Model fallback and load sharing.** A Model can name other Models in `fallback` and extra servers in
  `additionalEndpoints`. When a server answers 5xx or 429, the next server or Model answers the request. Several
  servers of one Model share the requests. Access rules, budgets and the audit record follow the Model that
  answered. See [Fallback and several servers](docs/providers.md#fallback-and-several-servers).
- **Smaller MCP tool results.** An admin can set `resultCompression` on a `Server` (`off`, `lossless` or `shorten`).
  Smaller tool results mean fewer input tokens in every later turn. Measured on 2026-11-02 with opencode 2.1.4 and
  Codex 0.159.3 (model openai/gpt-4.1-mini, 3 runs): `shorten` used 49% fewer input tokens per session with opencode
  and 31% fewer with Codex; `lossless` used 16% and 3% fewer. In 20 test questions, no answer was lost with
  `lossless`, and `shorten` answered as well as no compression. It is off by default. See
  [Compressing tool results](docs/concepts.md#compressing-tool-results).
- **Savings page** (Evidence → Savings, admins only). It shows what compression and model routing saved in the last
  30 days, in estimated tokens and dollars, by day, group, server and router. It needs `evidence.enabled`. See
  [Savings](docs/savings.md).
- **Servers from an image, with each person's own sign-in.** A `Server` that runs an image over HTTP can now have a
  `connection`: each person signs in to the vendor with their own account, and Ambit passes that person's token to
  the container through ToolHive's proxy. A Server that runs an image can also set `env` (values or Secret keys) and
  `args` for its container.
- **Masking handles large tool results.** `ambit-guard` accepts tool results up to 32 MiB (before: 4 MiB) and no
  longer runs out of memory at about 12 MiB.
- **Two privacy fixes for Presidio users.** (1) Setting Presidio no longer turns off the built-in masks (Korean
  resident registration numbers, US Social Security numbers, card numbers, emails); the built-in masks run first
  and Presidio adds to them (issue #189). (2) While Presidio is set, JSON tool results are not compressed, because
  compressed JSON hid most names from Presidio: the share of names found fell from 93.2% to 10.0% (issue #207).
- **Fixes:** the Zoom preset no longer fails the nightly contract check by mistake, and Codex no longer logs
  `failed to decode models response` (the gateway answers its `/v1/models` request in the shape Codex reads).

### Upgrade notes

- **Upgrade both charts, `ambit-crds` first.** The CRDs of `Model`, `Server` and `ServerRequest` have new optional
  fields: `fallback` and `additionalEndpoints` on a Model; `resultCompression`, `env` and `args` on a Server and in
  the server part of a ServerRequest.
  An older controller does not know them. Existing objects need no change.
- **The `ambit-guard` memory limit is now 512 MiB** (before: 128 MiB), set by the new value
  `protection.memoryLimitMiB`. The memory request stays 32 MiB, so scheduling does not change. A large tool result can
  make the guard use up to 512 MiB on its node.
- **New chart values** (all optional, with defaults): `protection.memoryLimitMiB` (512),
  `protection.presidio.timeout` (`10s`), `evidence.summaryRetention` (`26280h`, three years),
  `console.savings.referenceInputPricePerMillion` (empty). `protection.presidio.anonymizerURL` is no longer used; an
  existing value is accepted.
- **With an external ClickHouse**, the Savings page needs more rights for the ClickHouse user: `CREATE VIEW`,
  `ALTER` (modify TTL) and `SELECT` on `system.tables`, in addition to what it had.
- **A Model with `baseURL`** (a server you host) that lists `additionalEndpoints` must give each of those entries its
  own `baseURL`. Ambit refuses the Model otherwise.

### Known limits

- The Savings page counts from the day the summaries are created after the upgrade. There is no backfill of older
  days. All its numbers are estimates, and routing savings do not subtract the classifier's cost (issue #216).
- Compression changes the text of a tool result. Codex sends the model `structuredContent`, not the text, so for a
  tool that returns both, compression does not help with Codex. opencode 2.x in its default mode sends neither.
- `ambit-guard` refuses tool results larger than 32 MiB.
- A `Server` that runs a `stdio` image cannot have a `connection`.
- The nightly contract check of the DocuSign preset fails, because DocuSign's server does not ask an unauthenticated
  request to sign in (issue #222). The preset itself passed the live check for this release.
- The console no longer offers the Databricks and PagerDuty presets: their test accounts stopped working, so they
  could not pass the live check before this release (issue #244). A `Server` you already made from either preset is
  not changed.

### All changes

- Changelog entries are one file per pull request in changelog/unreleased/.
- CI: the image is built in its own job, and e2e runs in more parallel parts; a full run takes about 17 minutes instead of 26.
- Docs: Compressing tool results now has measured savings with opencode and Codex, and says which part of a tool result each client sends to the model: Codex sends `structuredContent` and not `content[].text`, so compression of the text does not help for a tool that returns both.
- A Model's `fallback` answers a request for the Model's name only for people who may use both Models. A person who may use the fallback but not the Model gets 403 for the Model's name, whether the Model works or fails.
- When a Model has a `baseURL` (a server you host), every entry in `additionalEndpoints` needs its own `baseURL`. Ambit refuses the Model otherwise, so no prompt can reach the provider's public server by mistake.
- Guard: with `evidence.enabled`, `ambit-guard` writes one record to the audit trail for each tool result text it compresses (Server, type, bytes before and after, the caller's groups; no email, no content). The coming Savings page counts them. A new `AmbitGuardRecordsDropped` alert (with `alerts.enabled`) shows when records were dropped. No call is changed or slowed by this.
- Savings page (Evidence → Savings, admins only): what tool result compression and model routing saved in the last 30 days, in estimated tokens and dollars, for each day, group, server and router. Every figure is labelled as an estimate. Compression dollars use each group's own average input price, else `console.savings.referenceInputPricePerMillion`, else tokens only. One control, "Later turns that carry each result" (default 0), shows what a result that is sent again would add; Ambit does not measure that number. Routing is measured against the most expensive priced model each router can choose, and does not subtract the classifier's cost. It counts from the day the summaries were created; there is no backfill. See Savings in the docs.
- Evidence: with `evidence.enabled`, the console creates two daily summary tables in ClickHouse (`ambit_llm_daily_v1` and `ambit_compress_daily_v1`) and a read-only admin API `GET /api/savings` that reads them. The summaries hold counts only (no email, no content) and are kept for the new value `evidence.summaryRetention` (default `26280h`, three years). They count from the day they are created. With an external ClickHouse, the user also needs `CREATE VIEW`, `ALTER` (modify TTL) and `SELECT` on `system.tables`. The new value `console.savings.referenceInputPricePerMillion` sets the price used for groups without priced LLM requests. The page that shows this comes in the next release note.
- Tool result compression: an admin can set `resultCompression` (`mode: off`, `lossless` or `shorten`) on a `Server` to make its tool results smaller before the client stores them, so each later turn costs fewer tokens. It works on JSON, CSV and tables, YAML, logs and test output; other text is not changed. `lossless` removes only layout. `shorten` also leaves out repeated log lines, passing tests, YAML comments and `managedFields`, marks each cut, and tells the model to call the tool again with a narrower query. It is off by default, runs before masking, and a fault in a compressor never fails a call. See Compressing tool results in Concepts.
- Guard: `ambit-guard` serves Prometheus metrics (`ambit_guard_compress_*`) on port 8081, the chart adds a `PodMonitor` for them and an `AmbitGuardCompressionFailing` alert when `alerts.enabled`, and a panic in the guard now fails one call instead of ending the process (which made every MCP call fail until the pod restarted).
- Masking: ambit-guard now accepts tool results larger than 4 MiB (the gRPC limit is 32 MiB). With the default memory limit, results up to 10 MiB were masked in a test; larger ones can make the guard run out of memory (see Masking in Concepts).
- Masking with Presidio: a tool result is sent to the analyzer in a few large calls instead of two calls per string, and Ambit replaces the findings itself. The anonymizer is no longer called. The new value `protection.presidio.timeout` (default 10 s) limits all analyzer calls of one result.
- Masking with Presidio: the built-in masks (Korean resident registration numbers, US Social Security numbers, card numbers, emails) now run first, and Presidio adds to them. Before, setting Presidio turned the built-in masks off.
- A Server can now have a `connection` (each person's own sign-in) and an `image`, when the image talks HTTP (`transport: streamable-http` or `sse`). Before, `connection` needed a `url`. A `stdio` image with a `connection` is still refused, with a clearer message. The person's token reaches the container through ToolHive's proxy. The console's "request an app" form accepts the same combination.
- A `Server` that runs an image can now set `env` (inline values, or a key of a Secret) and `args` for its container. Names that ToolHive sets are refused.
- A `Server` that runs an image and has a `connection` can now have its tools listed: bind the Connection (`serverURL`) to the address of the server's ToolHive proxy (`http://mcp-<name>-proxy.<namespace>.svc:8080/mcp`). Before, such a server was refused by a bound Connection and its tools could not be read. The console is now allowed to reach the proxy of a server for this.
- A Model can list other Models in `fallback` and extra servers in `additionalEndpoints`: when a server returns a 5xx or 429 response, the request is answered by the next server or Model, and several servers of one Model share the requests. Access rules, budgets and the audit record follow the Model that answered. See Model providers.
- Masking with Presidio: while `protection.presidio.analyzerURL` is set, the guard no longer compresses JSON tool results (issue #207). Removing the spaces in JSON lowered the share of names Presidio finds from 93.2% to 10.0%, so names reached the client unmasked. JSON now keeps its original layout; logs, tables, YAML and test output are still compressed. The new outcome `skipped_presidio` of `ambit_guard_compress_results_total` counts these results.
- Masking: `ambit-guard` now uses less memory for a large tool result, and the new value `protection.memoryLimitMiB` (default 512) sets its memory limit. Before, a result of about 12 MiB made the guard run out of memory and restart, and every MCP call failed while it restarted. Results up to the 32 MiB message limit are now masked (see Masking in Concepts).
- Preset check: the nightly contract check accepts resource metadata that the server names on another host (Zoom does this), if the metadata's `resource` is the server's URL or a parent of it and the address is https. This fixes the false failure for the Zoom preset.
- Codex: the gateway answers Codex's request for the model list (`GET /v1/models?client_version=...`) with `{"models":[]}`, the shape Codex reads. Codex no longer logs `failed to decode models response`. Other clients get the same OpenAI list as before.
- Savings page: the group chart now shows each group's share of the total instead of dollars that looked larger than the total, and says that people in several groups count in each group. "Counted since" now shows the oldest day the summaries hold, so it never conflicts with the chart.
- Usage page: reads the daily LLM summary table for the days it holds completely, instead of scanning every audit record. The numbers are the same; the page is much faster with a large audit trail. Days before the summary began still come from the audit records.
- The console no longer offers the Databricks and PagerDuty presets in its app list. Their test accounts stopped working, so they could not pass the live check before this release. A `Server` you already made from either preset is not changed. They come back when they pass the live check again.

## 0.2.3 (2026-10-18)

- Access rules can name people (`users`, email addresses) as well as groups.
  People are matched by a verified email (`accessRules.usersRequireVerifiedEmail`, default true); `ambit check --production` warns (P12) when it is false.
  Upgrade the controller and the ambit-guard together (the chart does this). An older guard does not know `users` and does not apply argument limits for people listed there.
- The LLM gateway answers 404, not 403, for a path it does not serve (for example /v1/v1/messages from a doubled base URL, or /v1/messages/count_tokens), with or without a key; the body names the path.
- GitOps mode: an app request shows Pull request open until its pull request is merged, and only then Approved; if the pull request is closed without a merge, the request goes back to waiting and can be approved again.
- ambit check --deep no longer fails P4 and P11 when a provider answers 429 or 5xx: it tries the next Ready model (up to 3) and names the models it tried. If every model failed that way, the result is Unknown.
- With HTTPS on, plain HTTP requests to the MCP host now redirect to HTTPS, like the console and LLM hosts.
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
