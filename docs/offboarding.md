# Offboarding: which SaaS providers truly revoke

When the identity provider deactivates someone (SCIM), Ambit revokes each of their stored app grants at the
provider and keeps a record of the provider's answer. By default it sends a standard RFC 7009 revocation
request: `POST <revocationURL>` with the form fields `token` and `token_type_hint`, and the app's client ID and
secret as HTTP Basic authentication. Where an app has no revocation URL, Ambit only deletes its own copy of the
grant; a token the provider already issued keeps working until it expires.

Some providers use their own format. Ambit picks it from the revocation URL you enter for the app:

| Revocation URL to enter | Format Ambit sends |
|---|---|
| `https://api.github.com/applications/{client_id}/grant` (GitHub Enterprise Server: `https://<host>/api/v3/applications/{client_id}/grant`) | GitHub: `DELETE`, app credentials, `{"access_token": ...}`; removes the whole grant |
| `https://slack.com/api/auth.revoke` | Slack: the user's token revokes itself; Ambit reads `"ok"` in the answer |
| `https://api.notion.com/v1/oauth/revoke` | Notion: app credentials, `{"token": ...}`, header `Notion-Version: 2026-03-11` |
| `https://api.hubapi.com/oauth/v1/refresh-tokens` | HubSpot: `DELETE .../refresh-tokens/<refresh token>` |
| anything else | standard RFC 7009 |

For GitHub, Slack and Notion, which take the access token, Ambit first renews an expired token. These formats
come from each provider's documentation (checked 2026-10-01) and are **not tested against the real providers**,
which needs an account with each. Each provider's endpoint:

| Provider | Revocation endpoint | Works with a standard (RFC 7009) request? | Tokens already issued | What is removed |
|---|---|---|---|---|
| Google (Workspace) | `POST https://oauth2.googleapis.com/revoke` | Yes (likely: client authentication is not required, so Ambit's is extra) | Revoked at once, including the paired token | The whole grant |
| Salesforce | `POST https://<MyDomain>.my.salesforce.com/services/oauth2/revoke` | Yes (likely: same reason) | Revoking a refresh token revokes its access tokens | The token(s) |
| Linear | `POST https://api.linear.app/oauth/revoke` | Probably (client authentication not documented) | Access tokens expire after 24 hours | As requested |
| Zoom | `POST https://zoom.us/oauth/revoke` | Probably (`token_type_hint` support not documented) | Access tokens expire after 1 hour | Not documented |
| GitHub (OAuth App, GitHub App user tokens) | `DELETE https://api.github.com/applications/{client_id}/grant` | **No**: needs DELETE with a JSON body `{"access_token": ...}` | Removed with the grant | The whole grant (or one token with `/token`) |
| Slack | `GET` or `POST https://slack.com/api/auth.revoke` | **No**: the token itself as bearer | Revoked at once | The token only |
| Notion | `POST https://api.notion.com/v1/oauth/revoke` | **No**: JSON body and a `Notion-Version` header | Revoked at once | The token only |
| HubSpot | `DELETE https://api.hubapi.com/oauth/v1/refresh-tokens/{token}` | **No**: DELETE with the token in the path | Access tokens stay valid up to 30 minutes | The refresh token only |
| Microsoft 365 / Entra ID (Graph) | none | **No endpoint.** Only admin actions: `revokeSignInSessions` (all of the user's apps) or deleting the app's consent grant | Stay valid until they expire (about 1 hour) | — |
| Atlassian (Jira, Confluence Cloud) | none | **No endpoint.** Only the user can remove the app, in their account settings | Stay valid until they expire | — |

## What this means today

- Ambit revokes at the provider for **Google and Salesforce**, very likely for **Linear and Zoom** (standard
  format), and for **GitHub, Slack and Notion** (their own formats).
- For **HubSpot**, Ambit revokes the refresh token; the access token already issued works for up to 30
  minutes more. HubSpot offers no way to end it sooner.
- For **Microsoft and Atlassian**, no API can revoke a user's grant to a third-party app; deleting Ambit's
  copy is all that is possible, and existing access tokens expire on their own (about an hour).

Sources: [Google](https://developers.google.com/identity/protocols/oauth2/web-server#tokenrevoke),
[Salesforce](https://help.salesforce.com/s/articleView?id=xcloud.remoteaccess_revoke_token.htm),
[Linear](https://linear.app/developers/oauth-2-0-authentication),
[Zoom](https://developers.zoom.us/docs/integrations/oauth/),
[GitHub](https://docs.github.com/en/rest/apps/oauth-applications),
[Slack](https://docs.slack.dev/reference/methods/auth.revoke),
[Notion](https://developers.notion.com/reference/revoke-token),
[HubSpot](https://developers.hubspot.com/docs/api-reference/auth-oauth-v1/guide),
[Microsoft revokeSignInSessions](https://learn.microsoft.com/en-us/graph/api/user-revokesigninsessions),
[Microsoft oauth2PermissionGrant delete](https://learn.microsoft.com/en-us/graph/api/oauth2permissiongrant-delete),
[Atlassian](https://developer.atlassian.com/cloud/jira/platform/oauth-2-3lo-apps/).
