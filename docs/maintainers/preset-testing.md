# Preset testing (maintainers)

### The three test levels

| Level | Where it runs | Needs an account | What it checks |
|---|---|---|---|
| 1. Catalog | every CI run (`go test ./internal/presets`) | no | The catalog parses; ids are unique; URLs are valid after placeholders are filled; scopes follow RFC 6749; `setup` contains `{callbackURL}`; reserved `authorizeParams` refused; every verified preset has a `live` block. A pull request that changes a preset without raising its `version` fails. |
| 2. Contract | nightly, `.github/workflows/presets-contract.yml` | no | The vendor's server answers an unauthenticated request with 401 (or the statuses the preset lists under `contract`); published metadata still matches the preset; the token URL answers a dummy authorization-code request with a JSON OAuth error (any HTTP status; an HTML page fails). A JSON body with `code` or `message` also counts, if the status is 400 or 401 (Canva, Webex). A preset that fails two nights in a row opens one issue. |
| 3. Live | maintainer's laptop, `make preset-live` | yes, a test account per vendor | Sign-in, refresh, `initialize`, `tools/list`, and one read-only tool call. |

The contract check has three exceptions:

- A preset with `contract: { appEndpoints: true }` (Linear, Canva) is not compared with the authorization and token
  endpoints in the MCP server's published metadata. Those name the server's own registration service. The preset
  uses the endpoints of an app made in the vendor's developer portal, on purpose. Its token URL is still probed.
- A preset with placeholders (ServiceNow) is checked with the tenant in `test/presets/fixtures.yaml`. If that
  tenant's host does not exist, the result is `SKIP`, not a failure. Put a real test tenant there.
- An issuer in the metadata that differs from the authorization server URL (Webex) is printed as a note. The
  preset holds no issuer. The endpoints are still compared.

Run one preset at a time: `go run ./cmd/presetcheck contract <id>`, `make preset-live APP=<id>`. `make preset-live`
checks every verified preset. `APP=<id>` checks only that preset, updates its entry in `results.json` and marks the
file partial; the release gate refuses partial results, so run `make preset-live` without `APP` before a release.

Some vendors refuse `127.0.0.1` and `localhost` redirects (Zoom; presets with `live.tunnel`). For their browser
sign-in, start an https tunnel to `http://127.0.0.1:8765`, for example `lt --port 8765 --local-host 127.0.0.1`
(localtunnel) or `cloudflared tunnel --url http://127.0.0.1:8765`, register `https://<tunnel host>/callback` in the
vendor's test OAuth app, and paste the same address when the live check asks. The address is not saved; later
runs renew the token without a browser, so the tunnel is needed only for a new sign-in.

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
- All 18 initial presets are verified: github, linear, asana, monday, airtable, dropbox, canva, gmail,
  google-calendar, google-drive, google-docs, google-sheets, slack, zoom, hubspot, docusign, databricks,
  pagerduty.
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
   company-registered client, stop, write the reason in this file, and tell the product owner. "All 18" then
   needs a decision.

Changing an existing preset needs a higher `version`; CI checks this.
