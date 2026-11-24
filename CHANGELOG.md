# Changelog

This file lists the major changes of each release, what you must do when you upgrade, and the publicly known
vulnerabilities that the release fixes. The release page on GitHub also lists every merged pull request.
For upgrade steps in detail, read [docs/upgrading.md](docs/upgrading.md).

## Unreleased

Each pull request adds a file in changelog/unreleased/ (see its README); the release assembles them here.

## 0.5.0 (2026-11-24)

### What is new

- **Service accounts.** CI jobs and background agents get their own identity, `<name>@svc.ambit.internal`, with up
  to 2 keys. An admin creates one under **Settings → Service accounts** and gives it exactly one group. AccessRules,
  Budgets and Audit treat the account as one person of that group, and Audit marks its calls as `service account`.
  `allSignedIn: true` does not grant a service account (see the upgrade notes). See
  [Service accounts](docs/service-accounts.md).
- **Evidence exports for auditors.** On the new page **Settings → Evidence** (admins only), an admin downloads a ZIP
  file in three clicks: an access review for a date (who may use what), offboarding evidence for a period (no
  accepted call after a person left), or the audit log for a period. Each ZIP has CSV files and a manifest with the
  SHA-256 of each file. The access review needs `evidence.enabled` and covers dates from the upgrade to 0.5.0 on.
  MCP app requests and approvals are now audit records too. See [Compliance](docs/compliance.md#evidence-exports)
  and the new guide [Do a quarterly access review](docs/guides/access-review.md).
- **Prompt guard on the model path, off by default.** With `llm.promptGuard.enabled: true`, the gateway checks the
  prompt of every chat, messages and responses request before it reaches a model, with built-in patterns (card
  numbers, US Social Security numbers, email addresses, phone numbers), your own regular expressions and an optional
  webhook in your cluster. Mode `audit` (the default when turned on) only records; `reject` refuses with a JSON
  error; `mask` replaces the matches. A `ModelRouter` of type `LLM` sends its classifier only text that passed the
  guard. Nothing changes until you turn it on. See [Prompt guard](docs/concepts.md#prompt-guard).
- **The MCP guard is stricter.** It masks card numbers next to dates and times, more written forms of card numbers,
  resident registration numbers, US Social Security numbers and emails, and also object keys, numbers and `uri`
  values in structured content. It masks `resources/read` and `prompts/get` results too. Fewer ordinary numbers are
  taken for card numbers. A tool result that is not a JSON object is withheld.
- **Argument limits are harder to get around.** A limit holds for every letter case of the argument name, and
  `notContains` treats runs of white space and SQL comments as one space. A limit can set `min` next to `max`.
- **Tool pinning covers more of each tool:** its title, output schema and standard annotations. Approvals made
  before the upgrade carry over when the name, description and input schema are unchanged.
- **Security fixes.** The CLI and the console image are built with Go 1.27.4, which fixes GO-2026-6859
  (`net/http`) and GO-2026-6809 (`crypto/x509`). The console uses brotli v1.2.7 and compress v1.20.3, which fix
  GO-2026-6888 and GO-2026-6865. `govulncheck ./...` reports no called vulnerability.
- **Clearer errors for clients.** A model request with no key gets 403 with "authorization failed: no Ambit key or
  token was sent", in the API's own error format. A refusal by the prompt guard is a JSON error with code
  `prompt_guard`.
- **Command-line tool (`ambit`) for any cluster:** `ambit init` writes a `values.yaml` by asking questions (also for
  Argo CD), `ambit install` installs Ambit (`--dry-run` changes nothing), `ambit upgrade` upgrades in the right order
  and lists the upgrade notes that apply, and `ambit uninstall` removes it. They write ordinary Helm releases, so you
  can mix them with `helm`. Tested in CI on kind and k3s.
- **More coding tools:** `ambit connect hermes`, `kilocode` and `pi`, and opencode 2 support.
- **Docs:** the page "Install locally by hand" moved from `/docs/local-up` to
  [`/docs/install-by-hand`](docs/install-by-hand.md); the old address redirects to it.

### Upgrade notes

Read [Upgrading](docs/upgrading.md), section "To the release with service accounts (0.5.0)". `ambit upgrade` lists it
for you. In short:

- **No dependency change:** agentgateway stays at v1.7.0, and `crds.yaml` is the same as in 0.4.0. **The Ambit CRDs
  change:** AccessRule gains `arguments[].min`, and the Server status gains `tools[].legacyFingerprint`. Upgrade
  `ambit-crds`, then `ambit`, as before (`ambit upgrade` does both).
- **Tool approvals move to the new fingerprint by themselves.** The controller moves each approval when it next reads
  a server's tools (at start, then every 10 minutes), if the tool's name, description and input schema are the
  approved ones. A tool that changed stays hidden until an admin approves it again. The controller logs
  `approvals made before 0.5.0 moved to the new tool fingerprint`.
- **A shared server whose tools were never read is now closed** (`ToolsPinned=False`, reason `NotRead`) until the
  controller reads them. A server with `connection` whose tools no admin has read stays usable and shows
  "Tool pinning off".
- **`allSignedIn: true` grants every person, but no service account.** No service account exists before 0.5.0, so
  nothing changes at the upgrade. When you add a service account, name its group or identity in a rule.
- **Tokens with an email that ends in `@svc.ambit.internal` are refused** by the gateways, the console sign-in and
  SCIM.
- **New Helm values, all optional:**

  | Value | Default | What it does |
  |---|---|---|
  | `llm.promptGuard.*` | `enabled: false` | Prompt guard on the model path (mode, built-in patterns, your patterns, reject message, webhook) |
  | `serviceAccounts.enabled` | `true` | `false` is the emergency stop for service account keys at the gateways |
  | `serviceAccounts.maxDays` | `90` | Longest end date of a service account key: 30, 90 or 365 |
  | `serviceAccounts.exposureDays` | `7` | Days a service account key keeps working after the admin who issued it is offboarded (0 to 30) |
  | `evidence.exports.enabled` | `true` | The Settings → Evidence page and its API |
  | `evidence.exports.maxRows` | `5000000` | An export with more rows is refused (413) |
  | `evidence.exports.maxDays` | `366` | The longest period of one export |
  | `protection.compressionAudit.caller` | `true` | `false` keeps the caller's email and key ID out of compression records |

- **Some answers change.** A model request with no key gets a new 403 message and body. A prompt guard refusal is
  JSON (it was plain text). In audit-only mode, the summary's "refused" count no longer includes prompt guard
  refusals. The MCP guard masks more text, and withholds a tool result that is not a JSON object.
- **The audit log no longer stores the text of an MCP server's JSON-RPC error** (`mcp.error.message`), because it
  can hold personal data. `mcp.error.code` stays.
- **The console needs one more right**, to create Events (the chart adds it). With `evidence.enabled`, the console
  has a new metrics port, 8084.
- **Before a downgrade below 0.5.0:** delete service accounts first. After it, approve the tools again (the older
  guard does not know the new fingerprints), remove `min` from argument limits, and remove the email from compression
  records. See [Upgrading](docs/upgrading.md).

### Verify the image signature

With cosign 2.0 or newer:

```sh
cosign verify ghcr.io/craftsman-apps/ambit-console:0.5.0 \
  --certificate-identity-regexp '^https://github.com/craftsman-apps/ambit/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

### Known limits

- **winget and Scoop are not available yet.** The release workflow can publish to them, but the Scoop bucket and the
  first winget package are not set up, so the release skips both with a notice. On Windows, use `install.ps1` or
  npm. The first winget package also needs Microsoft's review, which takes days.
- The MCP guard does not see JSON-RPC errors, log and progress notifications, and sampling or elicitation requests
  from an MCP server; they reach the client unmasked. See [Compliance](docs/compliance.md#limits).
- The prompt guard checks prompts only, not answers or embeddings.
- A number above 2^53 in a tool result is masked from a rounded value, so its masked form can differ from the
  original in the last digits (issue #360).
- The Databricks and PagerDuty presets are still not verified and not offered in the console (issue #244).
- The benchmark run in which all MCP requests with masking failed is still not explained (issue #274).

### All changes

- CLI on Windows: `winget install CraftsmanApps.Ambit` and `scoop install ambit/ambit` (bucket `craftsman-apps/scoop-ambit`) install the same binary as the GitHub release, checked by SHA-256, once the maintainers have set up the Scoop bucket and the first winget package (not yet for 0.5.0; see Known limits) (#78).
- Release workflow: before any channel is written, `gh attestation verify` proves that `checksums.txt` was made by this repository's release workflow; a Windows job checks the zip, `install.ps1`, Scoop and the winget manifests (#78).
- CLI: `ambit install --dry-run` checks the cluster, renders both charts and lets the API server validate the dependency CRDs and every object whose kind exists, and lists the objects whose CRD is not installed yet; it changes nothing (#78).
- CLI: `ambit init` writes `values.yaml` for `ambit install` by asking questions (addresses, company sign-in, gateway tokens, TLS, audit log, budgets, tool protection, GitOps, replicas). It never asks for a secret value: it prints the `kubectl create secret` commands for the Secrets the values name (#78).
- CLI: `ambit init --gitops argocd` also writes the dependency CRDs and four Argo CD Applications (with an optional parent Application) into a configuration repository, so Argo CD installs Ambit (#78).
- CLI: `ambit install -f values.yaml` installs Ambit on any cluster: it shows the target and asks, checks the cluster, applies the dependency CRDs (never downgrades a newer Gateway API CRD, asks before it changes an existing one), installs the Helm releases `ambit-crds` and `ambit`, waits for the pods and runs the production checklist (exit 3 when a production check fails). `--secret-file <secret>/<key>=<path>` creates the Secrets the values name from files, outside Helm (#78).
- CLI: `ambit uninstall` removes the releases `ambit` and `ambit-crds` after you type the context name, and lists what stays in the cluster (CRDs, your objects, audit volumes) (#78).
- CLI: `ambit check` R2 checks every permission an install needs, also ClusterRoleBindings, admission policies and the objects in the namespace (#78).
- CI: `ambit install`, `upgrade` and `uninstall` are tested on k3s as well as on kind, so the CLI is proven on a cluster that is not kind (#78).
- CLI: two `ambit local up` or `ambit local down` runs for the same `--name` no longer run at the same time; the second stops at once with exit 1 (#107).
- CLI: with `--output json`, a failure prints `{"error": {"what", "why", "next", "code"}}` on standard output, and `ambit check --pre --local --output json` prints its JSON when the kind cluster cannot be checked (#107).
- CLI: `--verbose` output redacts every line of a multi-line secret, such as a PEM private key, and a secret that a long line splits across two pieces (#108).
- CLI: waiting for pods shows a Deployment whose rollout is in progress as `1/1 ready, 0/1 updated`, names workloads that have no pod next to the waiting pods, and names the cause instead of `context deadline exceeded` when the time runs out (#108).
- CLI: `ambit local up --name` refuses the names that Windows reserves (`con`, `nul`, `com1` and others) on every system (#108).
- CLI: the `--deep` audit check no longer counts the collector's `_created` series as sent records (#108).
- Release workflow: a re-run of an older stable tag is no longer marked as the latest release and is not sent to Homebrew or npm (#108).
- CLI: `ambit upgrade -f values.yaml` (or `--reuse-values`) upgrades Ambit to the CLI's version in the right order, finds releases installed with Helm under any name, lists the upgrade notes between the two versions, refuses a downgrade, and refuses a release left pending unless `--recover-pending` (15 minutes or older, asks first). `ambit upgrade` and `helm upgrade` can be mixed, and `helm rollback` works on the releases (#78).
- `ambit connect hermes`, `ambit connect kilocode` and `ambit connect pi` add Ambit as an OpenAI-compatible provider and make it the default model, and `ambit disconnect` removes it again. Hermes Agent (`~/.hermes/config.yaml`, provider `key_cmd`) and pi (`~/.pi/agent/models.json`, `apiKey` starting with `!`) run `ambit key print` to read the key, so no file holds it. Kilo Code cannot run a command for the key, and its VS Code extension starts Kilo in the background, so connect writes the key to `~/.config/ambit/kilo-key`, which only you can read, and `kilo.jsonc` refers to it with `{file:...}`.
- New flag `ambit connect --mcp-url <address>` for `hermes`, `kilocode` and `pi`: it also adds the Ambit MCP gateway as the MCP server `ambit`, with the same key (scope LLM and MCP). Hermes gets the `mcp-remote` bridge with the header file `~/.ambit/hermes-mcp-headers.txt` (mode `0600`), because Hermes reads MCP headers only from its config or its environment.
- `ambit key set` and `ambit connect --replace-key` also write the new key into the Kilo Code key file and the Hermes header file.
- Console, **Keys**: new tabs **Hermes Agent**, **Kilo Code** and **pi** under **Connect a tool**, on the new key and on the key's page, and a block for each of them on the MCP tab.
- Docs: [Connect your coding agents](docs/guides/coding-tools.md) has a section for Hermes Agent, Kilo Code and pi, with the steps by hand and the known limits.
- `ambit connect opencode` supports opencode 2. opencode 2 sends model calls from a background service that does not see the shell function, so the old setup sent no key and the gateway answered 403 "authorization failed". For opencode 2 (found with `opencode --version`), connect now writes the key to `~/.config/ambit/opencode-key`, which only you can read, and the provider reads it with `{file:...}`. It writes the opencode 2 format (`providers`), or the older `provider` format when your file uses it, adds no shell function, removes the one an earlier connect added, and tells you to run `opencode service restart`. `ambit disconnect opencode` deletes the key file. opencode 1 works as before.
- `ambit key set` also writes the new key into the files that `ambit connect` wrote for that address and that hold the key: the opencode 2 key file and the `claude-desktop-mcp` header file.
- Console, **Keys**: the opencode tab shows the opencode 2 steps (the key file, the config, the service restart), with opencode 1 under **Version**. The MCP tab's opencode block reads the key file too.
- Console, **Keys**: choose a key's name to open its page: name, scope, dates, last use, owner, `ambit key set`, and the **Connect a tool** steps again, with `ambit_...` where the key goes. The console still shows a key only once.
- **Prompt guard on the model path (`llm.promptGuard`), off by default.** The gateway checks the prompt of every chat, messages and responses request before it reaches a model, with patterns built into the gateway (card numbers, US Social Security numbers, email addresses, phone numbers), your own regular expressions, and an optional webhook in your cluster. Three modes: `audit` (the default when turned on: record what would be refused, change nothing), `reject` (403 with your message; the model is not called and the prompt is not charged to daily budgets) and `mask` (matches replaced before the model sees the prompt). The prompt text never leaves the cluster. The Audit page shows "Refused by prompt guard", "Prompt guard would refuse" and "Masked by prompt guard", apart from access-rule refusals; no record holds the matched text. Answers and embeddings are not checked. See [Prompt guard](docs/concepts.md#prompt-guard).
- **A `ModelRouter` of type `LLM` sends its classifier model only text that passed the prompt guard.** The gateway asks `ambit-router` for the label before it runs the prompt guard, so a prompt the guard refuses or masks used to reach the classifier model unchanged. With `llm.promptGuard` in mode `reject` or `mask`, `ambit-router` now applies the same patterns, built-in patterns and webhook, from the same Helm values, before it calls the classifier: a refused prompt is not sent (route reason `guard`), and a masked prompt is sent masked. In mode `audit` nothing changes. The webhook Service must accept connections from the `ambit-router` pods. See [Prompt guard](docs/concepts.md#prompt-guard).
- **The prompt guard's refusal is a JSON error.** Mode `reject` answers with `{"type": "error", "error": {"type": "permission_error", "code": "prompt_guard", "message": "<rejectMessage>"}}` and `content-type: application/json`, which OpenAI and Anthropic clients both read as an error (it was plain text).
- **Audit-only mode: the summary's "refused" count no longer includes refusals by the prompt guard.** They are not access refusals. The Audit page still lists them as "Refused by prompt guard".
- Audit: MCP app requests, approvals, denials, approvals of changed tool definitions, and refused attempts (for example a viewer who tries to approve, or a self-approval) are now audit records. They are in ClickHouse `otel_logs` (`ambit.kind = app-approval`, kept for `evidence.retention`), on the Audit page under the new search "MCP app requests and approvals" with CSV export, and in `GET /api/audit?approvals=true`. Each step also writes a Kubernetes Event on the `ServerRequest` or `Server` (#112).
- Alerts: `AmbitConsoleAuditSendFailed` fires when the console cannot send these records to the collector (metric `ambit_console_audit_send_failures_total`, on the new console metrics port 8084 with `evidence.enabled`).
- Each compression record (one per tool result text the guard shortened or compressed, with `evidence.enabled`) now names the caller and the call: the caller's email and key ID, the tool, the index of the text, and `ambit.call_id`, a random ID that the gateway's record of the same call also has. The record still holds no content. New Helm value `protection.compressionAudit.caller` (default `true`); `false` keeps the email and key ID out of the records. See [Compliance](docs/compliance.md#limits). Before a downgrade below 0.5.0, read [Upgrading](docs/upgrading.md).
- The Audit page shows which tool calls had their result shortened or compressed by the guard: a "shortened" or "compressed" tag on the call's row and the sizes before and after (for example "Result shortened: 37.2 KB → 9.8 KB (shorten, logs, 14 parts left out)"). `GET /api/audit` gives the same facts in a new field `compression`, and the Audit CSV has three new columns at the end: `result_bytes_in`, `result_bytes_out` and `compression`. A record of the guard is never a row of its own on the Audit page. See [Compliance](docs/compliance.md#where-the-evidence-is).
- Evidence exports, part 3: admins can download an access review for a date, `GET /api/evidence/access.zip?date=2026-12-31`: `grants.csv` (one row per AccessRule, who and what in force at the end of the date, from the controller's access snapshots, with argument limits, the rules version and the access mode), `keys.csv` (personal and service account keys alive at the end of the date), `people-seen.csv` (people whose calls carried each group in the 90 days before) and `manifest.json` (the snapshot used and every snapshot of the day). Group membership comes from your identity provider; the manifest says so. Access reviews are available from the upgrade to 0.5.0 on.
- Evidence exports, part 1: admins can download the audit log of a period as one ZIP file, `GET /api/evidence/audit.zip?from=2026-10-01&to=2026-12-31[&email=...]` (dates are whole UTC days, both included; at most `evidence.exports.maxDays`, default 366, and `evidence.exports.maxRows`, default 5,000,000 rows). The ZIP holds `calls.csv` (every gateway record, with the identity, key ID, rules version, prompt guard action and, for wrong-key requests, the client address), `daily-counts.csv` (records per UTC day, to make a gap visible), `app-requests.csv` (MCP app requests and decisions) and `manifest.json` (rows and SHA-256 of each file, the exact SQL, the audit retention read from the table and the oldest record kept). Each export is recorded with the ZIP's SHA-256; `GET /api/evidence` lists them. The Settings → Evidence page follows in a later change.
- Evidence exports, part 2: admins can download the offboarding evidence of a period, `GET /api/evidence/offboarding.zip?from=…&to=…`: `offboarding.csv` (one row per offboarding record, with the last accepted call before, the number of calls the gateway accepted from 5 s after the offboarding until the export, which should be 0, and the first refused call after), `grants-revoked.csv`, `calls-after.csv` (each accepted call after the offboarding; an empty file is the good result), `key-events.csv` (key and service account key revocations of the period) and `manifest.json` with each source's available period. Without `evidence.enabled` the audit columns are empty and the manifest says so.
- Evidence exports: the audit export ZIP gains `compression.csv` (one row per compression record of the guard: call ID, caller, server, tool, mode, sizes; never content), and `calls.csv` and `calls-after.csv` gain a last column `call_id`, which links a call to its compression records (#348).
- Console: new page **Settings → Evidence** (admins only). Pick an export (access review for a date, offboarding or audit log for a period; the default is the last full quarter) and download it in three clicks. The page shows the audit retention and the oldest record kept (in red when the period starts before it), says when a date has no access snapshot, shows the ZIP's SHA-256 after each download to send to the auditor, and lists the export history. New guide: [Do a quarterly access review](docs/guides/access-review.md). `docs/compliance.md` describes the three exports and points SOC 2 CC6.2, CC6.3, CC7.3 and ISO/IEC 27001 controls 5.18 and 8.15 to them.
- With `evidence.enabled`, the controller records the AccessRules in force in the audit store (`otel_logs`, `ambit.kind` = `access-snapshot`): when a rules or access mode change is active, when only an argument limit changes, at start, and once a day at 00:05 UTC. These records are the base of the coming evidence exports ("who may use what on date D"); see "Access snapshots" in `docs/compliance.md`. A new alert, `AmbitControllerRecordsDropped`, fires when a record could not be sent.
- MCP guard: a card number is now masked when a date, time or other short number stands next to it, as in a log line `2026-11-17 10:00:01 4111 1111 1111 1111` or a card followed by its expiry date or CVV (#295).
- MCP guard: the default masks find more written forms: card numbers with dots, runs of spaces or full-width digits; foreign resident registration numbers (7th digit 5 to 8); resident registration numbers and US Social Security numbers without a dash or with spaces, when a label such as `RRN`, `주민등록번호` or `SSN` stands just before them; and emails URL-encoded as `%40` (#296). The placeholders are the same (`<NATIONAL_ID>`, `<CARD_NUMBER>`, `<EMAIL>`); more text is masked.
- MCP guard: fewer ordinary numbers are masked as card numbers: a number that starts with 0, 1, 7, 8 or 9 (for example a Unix time in milliseconds) is not a card number, and a number written in groups must start with a group of 4 digits and use one kind of separator, as cards are printed (#296).
- MCP guard: a tool result that is not a JSON object is withheld instead of passed unchecked (#333).
- Audit log: the text of an MCP server's JSON-RPC error (`mcp.error.message`) is no longer stored in ClickHouse or written to the gateway's log, because it can hold personal data the guard does not see; `mcp.error.code` stays (#335).
- Docs: concepts.md and compliance.md list the written forms each default mask finds, and say that JSON-RPC errors, log and progress notifications, and sampling or elicitation requests from an MCP server reach the client unmasked, because the gateway sends only tool results to the guard (#333).
- Argument limits: a limit on an argument now also holds for every letter case of its name (`Query`, `QUERY` for `query`), because MCP servers written in Go read those as the same argument (#334).
- Argument limits: `notContains` treats runs of spaces, tabs and new lines, and SQL comments (`/* ... */`), as one space, so `DROP  TABLE` and `DROP/**/TABLE` are refused by `notContains: [DROP TABLE]`. The text inside a comment is checked too (#301).
- MCP guard: masking now also reads object keys and numbers in structured content and `uri` values; a masked number becomes the placeholder text, and a result whose keys mask to the same text is withheld (#298).
- MCP guard: results of `resources/read` and `prompts/get` are masked like tool results; the gateway sends them to the guard (#297).
- MCP guard: when the guard cannot read a `Server`, its tools are hidden and calls refused instead of passed without tool pinning (#300).
- Tool pinning: a shared server (no `connection`) whose tools have not been read yet now has its tools hidden and refused until they are read. A server with `connection` whose tools no admin's account has read stays usable, and now shows that tool pinning is off: condition `ToolsPinned=False`, reason `NotPinned`, and "Tool pinning off" on the Servers page (#300).
- Argument limits: a limit can now set `min` (a number at least this value) next to `max`. A limit with only `max` still allows 0 and negative numbers, as before. The access rules page offers "at least" (#302).
- Tool pinning now also covers a tool's title, output schema and standard annotations (`readOnlyHint`, `destructiveHint` and the others), so a server that changes them after approval has the tool hidden until an admin approves it again. Approvals made before the upgrade carry over when the tool's name, description and input schema are unchanged; see Upgrading (#299).
- Service accounts (#83): identities for CI jobs and background agents, named `<name>@svc.ambit.internal`. An admin creates one under **Settings → Service accounts** with exactly one group and a description, and gives it up to 2 keys (ordinary `ambit_` keys, shown once, with environment variable and GitHub Actions steps). AccessRules (`groups` or `users`), Budgets and Audit see the account as one person of its group; Audit marks its calls as `service account`. Its keys do not stop for inactivity and are not touched when a person is offboarded, except that a key stops within 7 days when the admin who issued it is offboarded (rotate it). Names are never reused. The account page warns about keys not used for 30 days. See docs/service-accounts.md.
- New console endpoints for admins: `GET` and `POST /api/admin/service-accounts`, `DELETE /api/admin/service-accounts/{name}` and `POST /api/admin/service-accounts/{name}/keys`. Keys are revoked with the existing `DELETE /api/admin/keys/{id}`.
- New Helm values: `serviceAccounts.enabled` (default `true`; `false` is the emergency stop for service account keys at the gateways), `serviceAccounts.maxDays` (default `90`) and `serviceAccounts.exposureDays` (default `7`).
- Changed: `allSignedIn: true` in an AccessRule grants every person, but no service account. A service account gets only what a rule names by its group or identity. See docs/upgrading.md.
- The gateways refuse a token whose email (or, with `requireEmail: false`, whose subject) ends with `@svc.ambit.internal`; the console refuses such a sign-in, and SCIM refuses such a user.
- Security: the CLI, the console image and the test images are built with Go 1.27.4 (was Go 1.27.2 for the CLI and Go 1.27.3 for the console image). This fixes GO-2026-6859 (`net/http`) and GO-2026-6809 (`crypto/x509`). The console also uses `github.com/andybalholm/brotli` v1.2.7 and `github.com/klauspost/compress` v1.20.3 (through clickhouse-go v2.51.0, #309), which fix GO-2026-6888 and GO-2026-6865. `govulncheck ./...` reports no called vulnerability (#357).
- **A model request with no key says so.** A request to the LLM gateway with no Ambit key and no token (or an empty one, such as `Authorization: Bearer ` with nothing after it) still gets 403, now with the message "authorization failed: no Ambit key or token was sent", which says how to send a key and where to make one. The body is in the API's error format (Anthropic's for `/v1/messages`, OpenAI's for the others). A wrong key, a wrong token and a model the access rules refuse keep their answers (#321).
- The docs page "Install locally by hand" moved from `/docs/local-up` to `/docs/install-by-hand`, so it no longer reads as the page of `ambit local up`. The old address redirects to the new one (#320).
- `ambit connect opencode` with opencode 1, after a connect for opencode 2, now replaces Ambit's opencode 2 entry and deletes the opencode 2 key file. Before, it stopped with "uses the opencode v2 config format" (#305).
- A failed `ambit connect` no longer leaves an empty folder that it created for a new file (#305).
- `ambit connect` stops when a config file is a link to a file that does not exist, and says so; before, it replaced the link with a plain file (#257).

## 0.4.0 (2026-11-16)

### What is new

- **agentgateway v1.7.0.** The gateway is agentgateway v1.7.0 (was v1.6.0). A model call that the gateway refuses
  itself (403 for a model the person may not use, 404 for a model that does not exist) now shows on the Audit page
  the person who made the call; before, these rows had no person (issue #87).
- **Per-minute request limits.** A Budget can now limit requests per clock minute (UTC):
  `userRequestsPerMinute` (each person) and `groupRequestsPerMinute` (the whole group). A request counts when it
  arrives, also when it is then refused, so many requests sent at the same time all count. The console's Budgets page sets them
  (issue #85). See [Budgets](docs/budgets.md).
- **More on the Audit page.** A refused model call names the model the person asked for, `/v1/models` calls show as
  "Model list", and a second search lists requests made with a wrong, revoked or expired key (issue #117). In
  audit-only mode, a served call that no access rule judges is shown as "Served, no verdict" (issue #136).
- **Model keys in the console.** An admin can change a model's API key (**Change key**) and delete a model
  (**Delete**) on the Models page, also in GitOps mode (issues #166, #167). Add model and Change key accept AWS keys
  for Bedrock and a Google service-account key file for Vertex AI (issue #168). The console never shows a stored key.
- **The Savings page subtracts the classifier's cost.** For a ModelRouter of type `LLM`, the routing saving is now
  net of what the classifier calls cost; a router whose classifier costs more than it saves shows a negative saving
  (issue #216).
- **Command-line tool (`ambit`):**
  - `ambit key set --url <address>` stores a key in the keychain without connecting a tool.
  - `ambit connect claude-desktop-mcp --url <MCP address>` connects Claude Desktop through `mcp-remote`;
    `ambit disconnect claude-desktop-mcp` removes it.
  - `ambit connect` and `ambit disconnect` are safer: a move to a new address deletes the old key when no tool uses
    it, two runs at the same time do not lose state, and `ambit disconnect --revoke` revokes each deleted key at the
    console, so it stops working at once (the console's new endpoint `DELETE /api/keys/self`).
  - `ambit lint <file or directory>` warns on AccessRules that give wide access.
- **Signed images and npm trusted publishing.** The console image is signed with cosign (keyless, Sigstore) from
  this release; see "Verify the image signature" below. The npm packages are published through npm trusted
  publishing (GitHub OIDC).
- **Docusign preset version 2**, for Docusign's MCP server that now answers a request without a token with HTTP 401.
  An app made from version 1 does not change.
- **New page: [Performance](docs/performance.md).** On one reference laptop with a local kind cluster (2026-11-10),
  the gateway added 1.3 ms (personal API key) and 1.4 ms (company token) at the median (p50) to a model call at 10
  requests per second, and 1.4 to 4.3 ms at the median to an MCP tool call. These are numbers from one machine, not a guarantee; `test/bench.sh`
  repeats the measurement on your cluster.
- **New guide: [Run agents in a sandbox such as OpenShell](docs/guides/sandbox-openshell.md).** It makes the Ambit
  gateway the only model endpoint of an agent in an OpenShell sandbox.

### Upgrade notes

- **Apply the new dependency CRDs first** (`crds.yaml` of this release, with agentgateway v1.7.0's CRDs), then
  upgrade `ambit-crds`, then `ambit`. See [Upgrade and uninstall](docs/install.md#upgrade-and-uninstall) and
  [Upgrading](docs/upgrading.md). The agentgateway CRDs only gain fields, so objects you have stay valid.
- **The Budget CRD has two new optional fields**, `userRequestsPerMinute` and `groupRequestsPerMinute`. Existing
  Budgets need no change, and an older controller does not know the new fields.
- **The policy `ambit-llm-usage` is removed.** The controller deletes this generated AgentgatewayPolicy during the
  upgrade (issue #261). agentgateway v1.7.0 reports token usage for every streamed request itself, so dollar limits
  still charge streamed answers.
- **Embeddings requests count against daily request limits after the answer**, like other requests. An embeddings
  request that the gateway refuses (403 or 429) no longer counts. Per-minute limits count every request when it
  arrives.
- **Some answers change.** A tool the person may not use is answered `unknown resource` (before: `Unknown tool:
  <name>`). Gateway errors no longer include internal details; the status codes are the same. Every streamed chat
  request ends with a usage chunk, also when the client set `stream_options.include_usage` to false. `/v1/messages`
  requests to OpenAI's own API are sent as Responses requests. The text of a Budget refusal (429) no longer says
  "daily", and `Retry-After` is the reset of the limit that refused.
- **No new chart values.** The console needs two more rights, which the chart adds: delete Models, and patch the
  model key Secrets.
- **With an external ClickHouse**, the console creates a third summary table, `ambit_classifier_daily_v1`, and its
  view. The user needs no rights beyond those of 0.3.0.
- **Audit rows written before the upgrade keep their caller-less model refusals.** Only refusals after the upgrade
  show the caller.

### Verify the image signature

With cosign 2.0 or newer:

```sh
cosign verify ghcr.io/craftsman-apps/ambit-console:0.4.0 \
  --certificate-identity-regexp '^https://github.com/craftsman-apps/ambit/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The command fails for an image that the release workflow did not sign on a `v*` tag. Releases before 0.4.0 are not
signed. See [Supply chain](docs/compliance.md#supply-chain).

### Known limits

- The Databricks and PagerDuty presets are still not verified and not offered in the console: their test accounts
  do not work (issue #244).
- In one benchmark run (MCP tool calls with masking, 50 requests per second), all 3,000 requests through the gateway
  failed. The other runs of the same case had no errors, and the cause is not known (issue #274).
- The test mock model in `test/llm/mock.yaml` does not end a streamed answer with `finish_reason`, so a coding agent
  pointed at it sends requests without end (issue #275). Real providers are not affected; this is a test tool only.

### All changes

- Audit page: in audit-only mode, a served model request or tool call that no access rule judges (its mark is `n/a`) is now shown as a call, "Served, no verdict", instead of being sorted with protocol messages.
- `ambit connect claude-desktop-mcp --url <MCP address>` writes the `mcp-remote` entry to `claude_desktop_config.json` and the key to the header file `~/.ambit/mcp-headers.txt` (mode 0600); `ambit disconnect claude-desktop-mcp` removes both.
- `ambit connect` to a new `--url` deletes the stored key of the old address when no tool uses it any more, and prints the line to revoke it.
- `ambit connect`, `disconnect` and `key set` take a lock file, so two runs at the same time do not lose a record of the state file.
- `ambit disconnect opencode` without a state file looks for the shell function in the bash, zsh and fish profiles, not only in the profile of the current `$SHELL`.
- `ambit disconnect` prints each deleted key at once, so a failure to delete a later key does not hide the earlier ones.
- A key stored with `ambit key set` stays when the last tool that uses its address is disconnected or moved to another address; only `ambit disconnect --all` deletes it.
- Console: Add model and Change key accept AWS keys for Bedrock (access key ID, secret access key, optional session token) and a Google service-account key file for Vertex AI. The console stores them in the Secret `ambit-llm-<name>` and never shows them. Tested with fake keys only (#168).
- Console: an admin can change a model's API key on the Models page (**Change key**). The console writes the new key into the same Secret `ambit-llm-<name>` that it created when the model was added, also in GitOps mode, and never shows it. Secrets made outside the console are not changed (#166).
- Console: an admin can delete a model (**Delete**). The console refuses while an access rule, a model router or another model's fallback names the model. In Direct mode it deletes the Model and the key Secret the console created; in GitOps mode it opens a pull request, and the key is listed under **Keys without a model** once the Model is gone (#167).
- Console: **Unfinished additions** on the Models page is now **Keys without a model**. Remove key now also checks the Secrets of a model's `additionalEndpoints`.
- `ambit key set --url <address>` stores a key in the keychain without connecting a tool, so that `ambit key print` can serve a helper such as the `headersHelper` of a managed Claude Desktop.
- `ambit lint <file or directory>...` warns on AccessRules that give wide access: `allSignedIn: true`, and a server without a `tools` list (every tool, including tools added later). `--strict` exits 1 when there is a warning.
- Docusign preset version 2: the Docusign MCP server now answers a request without a token with HTTP 401 and OAuth resource metadata (before: 403). Nothing changes for an app made from version 1; its URLs and scopes are the same.
- Release images are signed with cosign (keyless, Sigstore) from 0.4.0. `docs/compliance.md` (Supply chain) shows the `cosign verify` command.
- The release publishes the npm packages through npm trusted publishing (GitHub OIDC); the stored npm token stays only as a fallback until a release has published without it.
- The console endpoint `DELETE /api/keys/self` revokes the key that is sent as the bearer token, and only that key. It reads no session cookie, is rate limited (30 a minute per console process), and writes the History action `revoke-self`.
- `ambit disconnect --revoke` revokes each key it deletes at the console, so a key stops working at once.
- Savings: the routing figure is now net of the classifier. For a ModelRouter of type `LLM`, ambit-router puts the classifier call's input and output tokens on the request, the audit record keeps them, and the Savings page subtracts their cost at the classifier Model's price. The router table shows the classifier's cost, and a router whose classifier costs more than it saves shows a negative saving. Classifier calls that time out, and days before this version, are not counted (#216).
- Embeddings requests now count against daily request limits after the answer, like all other requests. An embeddings request that the gateway refuses (403 or 429) no longer counts. Per-minute limits are unchanged: they still count every request when it arrives.
- Ambit no longer creates the policy `ambit-llm-usage`; the upgrade deletes it. The gateway (agentgateway v1.7.0) turns usage reporting on for every streamed request itself, so dollar limits still charge streamed answers.
- The Audit page names the model a person asked for when the gateway refused it (403 or 404), and shows `/v1/models` calls as "Model list" (issue #117).
- The Audit page has a second search: requests made with a wrong, revoked or expired key. They have no person; each row shows the time, path, status, the gateway's reason and the client address. The API is `GET /api/audit?invalidKey=true&from=&to=`.
- New page [Performance](docs/performance.md): the time the Ambit gateway adds to a model call and to an MCP tool call, measured on one reference machine, and `test/bench.sh` to repeat the measurement.
- The gateway is agentgateway v1.7.0 (was v1.6.0). Apply the new third-party CRDs before you upgrade the chart (`kubectl apply --server-side -f deploy/crds/`); see [Upgrading](https://ambitproject.dev/upgrading).
- A model call that the gateway refuses itself (403 for a model the person may not use, 404 for a model that does not exist) now appears on the Audit page under the caller. Before, these rows had no caller (issue #87).
- A tool call to a tool the person may not use is now answered `unknown resource` (it was `Unknown tool: <name>`), the same answer as for a tool that does not exist. The Audit page still shows it as refused by policy.
- Every streamed chat request now ends with a usage chunk (token counts), also when the client set `stream_options.include_usage` to false. Before, only people in a group with a dollar limit got it.
- `/v1/messages` requests to a Model with provider `OpenAI` and no `baseURL` (OpenAI's own API) are now sent to OpenAI as Responses requests, not Chat Completions. Thinking blocks from earlier turns are not sent to OpenAI in either format.
- Gateway error answers no longer include internal details such as addresses or upstream error text. The status codes are unchanged.
- New guide [Run agents in a sandbox such as OpenShell](docs/guides/sandbox-openshell.md): an OpenShell provider profile that makes the Ambit gateway the only model endpoint of an agent in a sandbox, tested with OpenShell v0.1.7 and opencode on 2026-11-10. The "When not to use Ambit" list says that Ambit does not confine what an agent does on a machine.
- Budgets can limit requests per minute: `userRequestsPerMinute` (each person) and `groupRequestsPerMinute` (the whole group), per clock minute (UTC). A request counts when it arrives, also when it is then refused, so a burst cannot get past the limit. The console's Budgets page sets them (issue #85).
- The text of a Budget refusal (HTTP 429) no longer says "daily": it covers per-minute and daily limits. `Retry-After` is the reset of the limit that refused: the rest of the minute for a per-minute limit, which is checked first, or the time to midnight UTC for a daily limit.
- The homepage shows the latest released version, read from `CHANGELOG.md` at build time, instead of a fixed `v0.1.0`. The README badges show the latest release and the license.

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
