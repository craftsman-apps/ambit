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

Maintainers: see [Preset testing](https://github.com/craftsman-apps/ambit/blob/main/docs/maintainers/preset-testing.md).

## Presets

`Revokes at vendor`: yes when Ambit can revoke a person's grant at the vendor when they leave (see
[offboarding.md](offboarding.md)). `Live-checked`: the date of the last passing live check.

| id | Vendor | Server | Revokes at vendor | Needs | Live-checked |
|---|---|---|---|---|---|
| github | GitHub | api.githubcopilot.com | yes |  | 2026-10-07 |
| linear | Linear | mcp.linear.app | yes |  | 2026-10-07 |
| asana | Asana | mcp.asana.com | yes |  | 2026-10-07 |
| monday | monday.com | mcp.monday.com | yes |  | 2026-10-07 |
| airtable | Airtable | mcp.airtable.com | no |  | 2026-10-07 |
| box | Box | mcp.box.com | yes |  | not yet |
| dropbox | Dropbox | mcp.dropbox.com | no |  | 2026-10-07 |
| canva | Canva | mcp.canva.com | no | Multi-factor authentication on the developer's Canva account | 2026-10-07 |
| slack | Slack | mcp.slack.com | yes |  | 2026-10-07 |
| zoom | Zoom | mcp.zoom.us | yes |  | 2026-10-07 |
| webex | Webex | mcp.webexapis.com | no | Control Hub admin enables the server | not yet |
| hubspot | HubSpot | mcp.hubspot.com | yes |  | 2026-10-07 |
| salesforce | Salesforce | api.salesforce.com | yes | Enterprise Edition or above, or a Developer Edition org | not yet |
| docusign | Docusign | mcp.docusign.com | no | Integration key (Go-Live review for production) | 2026-10-07 |
| servicenow | ServiceNow | your instance (service-now.com) | yes | MCP Server Console, Zurich Patch 9 or Australia Patch 2 or later | not yet |
| databricks | Databricks | your workspace host | no | SQL warehouse; account admin creates the app connection | 2026-10-07 |
| pagerduty | PagerDuty | mcp.pagerduty.com | yes | Advanced Permissions | 2026-10-07 |
| gmail | Gmail | gmailmcp.googleapis.com | yes | Google Workspace Developer Preview Program; Workspace organization (Internal consent screen) | 2026-10-08 |
| google-calendar | Google Calendar | calendarmcp.googleapis.com | yes | Google Workspace Developer Preview Program; Workspace organization (Internal consent screen) | 2026-10-08 |
| google-drive | Google Drive | drivemcp.googleapis.com | yes | Google Workspace Developer Preview Program; Workspace organization (Internal consent screen) | 2026-10-08 |
| google-docs | Google Docs | docsmcp.googleapis.com | yes | Google Workspace Developer Preview Program; Workspace organization (Internal consent screen) | 2026-10-08 |
| google-sheets | Google Sheets | sheetsmcp.googleapis.com | yes | Google Workspace Developer Preview Program; Workspace organization (Internal consent screen) | 2026-10-08 |

PayPal is not in the initial list: its MCP server accepts only clients that register themselves (dynamic client
registration), which Ambit adds in phase 2.

Salesforce is in the catalog but not in the initial list (phase 2). Its hosted MCP servers (Setup, MCP Servers,
"Salesforce Servers") exist only in Developer Edition orgs and Enterprise Edition or above; trial orgs show only
"External servers". The preset is not live-checked yet.

Box, Webex and ServiceNow are in the catalog but not in the initial list (phase 2); none is live-checked yet:

- Box: the MCP server integration is configured in the Box Admin Console, which needs a Business plan or above.
- Webex: a Control Hub admin must turn on the Messaging MCP server; free Webex accounts have no Control Hub.
- ServiceNow: needs an instance with the MCP Server Console; free developer instances were unavailable (waitlist).
