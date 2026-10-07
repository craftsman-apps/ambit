# App presets

A preset is a ready entry for a work app (GitHub, Slack, Salesforce and others) whose vendor runs its own MCP
server with per-user sign-in (OAuth). It holds the vendor's URLs, scopes and setup steps, so an admin does not
type them. Every preset shown in the console has passed a live test with a real test account. A preset that has
not passed is not shown.

A preset does not hold secrets. The company registers its own OAuth app at the vendor and enters that app's
client ID and secret in Ambit.

## Adding an app

Open Apps, then "Add an app". There are four screens.

1. **Choose.** A grid of presets with a search box and category buttons. Each card can show `tested <date>`,
   `preview` (the vendor's server is in preview) and `needs: ...` (an access program or plan the vendor
   requires). "Custom app..." opens the manual form.
2. **Set up.** Two parts.
   - "At <vendor>": numbered steps to register an OAuth app. The steps contain the callback URL of your Ambit
     install, with a Copy button.
   - "In Ambit": values the preset needs from you (for example your Salesforce My Domain or ServiceNow
     instance name), a name, the name users see, the client ID and the client secret. "Advanced" shows the
     MCP server URL and the scopes. Changing either is allowed; the app is then a custom app. The screen says
     whether Ambit can revoke a person's access at that vendor when they leave (see
     [offboarding.md](offboarding.md)).
3. **Add.** Ambit creates the client secret, a Connection and a Server together. In GitOps mode it opens one
   pull request that holds both files. The Connection carries the server URL and the resource, so it is bound
   to that server.
4. **Pick tools.** After the app is added and you have connected your own account, you read the tool list (see
   "The tool reader"), tick the tools to allow (all unticked at first), choose groups, and create an access
   rule. "Skip" creates no rule. The result line states who has access, counted from all rules.

Rules and limits:

- A Connection that has a server URL is bound. Any other Server that names it is refused: `Ready=False`,
  reason `ConnectionBound`.
- Changing the server URL or the resource of a connection after users have connected makes their grants
  "reconnect": each user signs in again.
- `authorizeParams` (extra authorize query parameters) allows at most 16 entries. These keys are refused:
  `client_id`, `redirect_uri`, `state`, `code_challenge`, `code_challenge_method`, `scope`, `resource`,
  `response_type`, `response_mode`, `request`, `request_uri`.
- Production presets accept `https` URLs only. A testing preset (`AMBIT_TEST_PRESETS`, Helm value
  `console.testPresets`) may also use plain `http`, only for `localhost` and hosts under `localtest.me`, `.svc` or `.svc.cluster.local`
  hosts.
- For apps whose tokens do not expire (for example GitHub), Ambit cannot tell when the app cancels a token.
  If the agent loses access, choose Connect again on the Connections page.

### Removing an app

- Direct mode: "Remove app" also removes the Server that the same preset addition created.
- GitOps mode: the Server stays in Git. Remove it with a second pull request. This is a known limit.

### Unfinished additions

An addition can stop half way (for example the console restarts after it created the secret). The Apps page
lists these as unfinished additions with "Finish" and "Remove".

- Direct mode hides additions younger than 10 seconds, because they may still be running.
- Removing a complete addition is refused; use "Remove app" instead.
- In GitOps mode "Remove" is offered only when no open or merged pull request uses the branch.

## The tool reader

A per-user server has no shared account, so Ambit cannot list its tools by itself. An admin lends their own
account for this.

- On the Servers page, an admin clicks "Use my account to read tools". Only that admin, from their own
  session, can give this consent.
- The console lists the server's tools with that account and shows names, descriptions and the server's own
  hints (`read-only`, `can delete or change data`). The hints are claims of the server, not checked by Ambit.
- Every admin can see the tool descriptions on the Servers page, with "Tools read with <email>'s account" and
  the time. The consent button says so.
- There is one reader per server. A new consent replaces the old one.
- Consent ends when the reader disconnects the app, reconnects it, is offboarded, or when the server URL
  changes. Any admin can clear it with "Clear".
- The controller gets tool metadata only, never tokens, from the console on port 8083. The network policy
  admits only the controller to that port.
- With no usable reader the controller keeps the last list and sets `ToolsRead=False`, reason
  `ReaderUnavailable`; the Servers page asks for another reader.

Known limit: with protection on, a tool that is hidden from one user's list can still be called by name, if
the vendor shows that user a different definition than it shows the reader. Calls are checked against the
reader's approved definition. This matters only for vendors that show different definitions to different
users. The live check reports such vendors when a second test account is configured.

## For maintainers

### The three test levels

| Level | Where it runs | Needs an account | What it checks |
|---|---|---|---|
| 1. Catalog | every CI run (`go test ./internal/presets`) | no | The catalog parses; ids are unique; URLs are valid after placeholders are filled; scopes follow RFC 6749; `setup` contains `{callbackURL}`; reserved `authorizeParams` refused; every verified preset has a `live` block. A pull request that changes a preset without raising its `version` fails. |
| 2. Contract | nightly, `.github/workflows/presets-contract.yml` | no | The vendor's server answers an unauthenticated request with 401 (or the statuses the preset lists under `contract`); published metadata still matches the preset; the token URL answers a dummy authorization-code request with a JSON OAuth error (any HTTP status; an HTML page fails). A preset that fails two nights in a row opens one issue. |
| 3. Live | maintainer's laptop, `make preset-live` | yes, a test account per vendor | Sign-in, refresh, `initialize`, `tools/list`, and one read-only tool call. |

Run one preset at a time: `go run ./cmd/presetcheck contract <id>`, `make preset-live APP=<id>`. `make preset-live`
always checks every verified preset; `APP=<id>` adds one preset that is not verified yet.

### Live check

- Credentials are in `~/.config/ambit/presetcheck.json`, mode 0600. They are never in Git or CI. The command
  refuses a symlink or a wider mode.
- It refuses to run with uncommitted changes under `internal`, `cmd`, `go.mod` and `go.sum`, because the result
  names a commit.
- A rotated refresh token is saved to the file before any MCP call.
- With no token, or after `invalid_grant`, it opens the browser for sign-in. The callback is
  `http://127.0.0.1:8765/callback`, or `https://127.0.0.1:8766/callback` for a preset with `live.callback:
  https` (the browser warns once about the self-signed certificate). Register that address as a second
  redirect URI in the test OAuth app.
- For a second test account (to compare tool definitions between users), use a separate browser profile so
  the vendor does not reuse the first session.
- Only the read-only tool named in `live.tool` is called.
- `test/presets/results.json` is committed. It holds the date, the commit, the catalog hash and each preset's
  result. For a failure it may hold up to 200 characters of the vendor's tool-error text. That text can
  include the test account's email address. Check it before you commit.
- `test/presets/tools/<id>.txt` lists the tool names. A change is reported, not failed.

### Release gate

`go run ./cmd/presetcheck gate` runs in the release workflow, which checks out the full history. It refuses
the release unless all of this holds:

- `results.json` is at most 7 days old and not dated in the future.
- It names a full 40-character commit id that is an ancestor of the release commit.
- Nothing under `api/v1alpha1`, `internal/console`, `internal/presets`, `cmd/presetcheck`,
  `internal/controller/tools.go`, `go.mod` or `go.sum` has changed since that commit. The one exception is the `verified` and `verifiedOn`
  fields of the catalog.
- The catalog hash matches the one in `results.json`.
- Every preset with `verified: true` passed at its current `version`.
- All 22 initial presets are verified: github, linear, asana, monday, airtable, box, dropbox, canva, gmail,
  google-calendar, google-drive, google-docs, google-sheets, slack, zoom, webex, hubspot, salesforce,
  docusign, servicenow, databricks, pagerduty.
- The clone is not shallow. Any Git error fails the gate; it never skips the check.

Setting `verified: false` is a deliberate change in Git. The release never does it.

### Adding a new preset

Claude does not sign up for or sign in to vendor accounts; the maintainer does steps 2 and 5.

1. Write the entry in `internal/presets/presets.yaml` from the vendor's current documentation, without
   `verified`. Put the documentation URL in `docs`. Write `setup` steps that can be followed literally; each
   step that registers the app includes `{callbackURL}`. Choose a `live.tool` that only reads. Add the
   placeholder values for the contract check to `test/presets/fixtures.yaml`.
2. (Maintainer) Create the test account and register the OAuth app by following the preset's own `setup`
   text. Add the live callback as a second redirect URI.
3. Run `go run ./cmd/presetcheck contract <id>`. Fix the entry until there are no problems.
4. Run `go test ./internal/presets -count=1`.
5. Commit the entry as "Preset: <name> (entry, not yet live-checked)".
6. (Maintainer) Run `make preset-live APP=<id>` on a clean tree. Expected: `PASS <id>`, and
   `test/presets/tools/<id>.txt` written.
7. Set `verified: true` and `verifiedOn: <date>`. Both are left out of the catalog hash, so the live result
   stays valid. Add the vendor's row to the table below. Commit with `test/presets`.
8. If a vendor refuses access (the Google preview program, Canva) or its server does not work with a
   company-registered client, stop, write the reason in this file, and tell the product owner. "All 22" then
   needs a decision.

Changing an existing preset needs a higher `version`; CI checks this.

## Presets

`Revokes at vendor`: yes when Ambit can revoke a person's grant at the vendor when they leave (see
[offboarding.md](offboarding.md)). `Live-checked`: the date of the last passing live check.

| id | Vendor | Server | Revokes at vendor | Needs | Live-checked |
|---|---|---|---|---|---|
| github | GitHub | api.githubcopilot.com | yes |  | 2026-10-07 |
| linear | Linear | mcp.linear.app | yes |  | not yet |
| asana | Asana | mcp.asana.com | yes |  | 2026-10-07 |
| monday | monday.com | mcp.monday.com | yes |  | 2026-10-07 |
| airtable | Airtable | mcp.airtable.com | no |  | 2026-10-07 |
| box | Box | mcp.box.com | yes |  | not yet |
| dropbox | Dropbox | mcp.dropbox.com | no |  | not yet |
| canva | Canva | mcp.canva.com | no | Canva MCP access for the developer app | not yet |
| slack | Slack | mcp.slack.com | yes |  | not yet |
| zoom | Zoom | mcp.zoom.us | yes |  | not yet |
| webex | Webex | mcp.webexapis.com | no | Control Hub admin enables the server | not yet |
| hubspot | HubSpot | mcp.hubspot.com | yes |  | not yet |
| salesforce | Salesforce | api.salesforce.com | yes | Enterprise Edition or above, or a Developer Edition org | not yet |
| docusign | Docusign | mcp.docusign.com | no | Integration key (Go-Live review for production) | not yet |
| servicenow | ServiceNow | your instance (service-now.com) | yes | MCP Server Console, Zurich Patch 9 or Australia Patch 2 or later | not yet |
| databricks | Databricks | your workspace host | no | SQL warehouse; account admin creates the app connection | not yet |
| pagerduty | PagerDuty | mcp.pagerduty.com | yes | Advanced Permissions | not yet |
| gmail | Gmail | gmailmcp.googleapis.com | yes | Google Workspace Developer Preview Program; Workspace organization (Internal consent screen) | not yet |
| google-calendar | Google Calendar | calendarmcp.googleapis.com | yes | Google Workspace Developer Preview Program; Workspace organization (Internal consent screen) | not yet |
| google-drive | Google Drive | drivemcp.googleapis.com | yes | Google Workspace Developer Preview Program; Workspace organization (Internal consent screen) | not yet |
| google-docs | Google Docs | docsmcp.googleapis.com | yes | Google Workspace Developer Preview Program; Workspace organization (Internal consent screen) | not yet |
| google-sheets | Google Sheets | sheetsmcp.googleapis.com | yes | Google Workspace Developer Preview Program; Workspace organization (Internal consent screen) | not yet |

PayPal is not in the initial list: its MCP server accepts only clients that register themselves (dynamic client
registration), which Ambit adds in phase 2.
