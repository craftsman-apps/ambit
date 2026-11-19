---
title: Compliance and audits
description: How Ambit's features map to SOC 2, ISO/IEC 27001:2022 and EU AI Act controls, which evidence you can show an auditor today, and what Ambit does not cover.
---

# Compliance and audits

Ambit runs in your cluster, so your audit covers it, in the same way it covers the rest of your systems. Ambit has no SOC 2 report and no ISO certificate of its own, and it does not plan to pursue one now. This page helps you pass **your** audit. It maps what Ambit does to the controls that auditors ask about, and it says where the evidence is today.

Three points to keep in mind:

- Ambit supports a control. It does not satisfy a control by itself. Your auditor judges your whole system: your identity provider, your cluster, your processes and your people.
- Evidence today is gathered by hand: console pages, ClickHouse queries, Kubernetes objects in Git, and Helm values. A ready-made evidence export is planned. It is not shipped.
- Ambit is Alpha software (see [Status](index.md#status-alpha)). Read [Limits](#limits) before you give any of this to an auditor.

This page is not legal advice.

## Where the evidence is

| Source | What it holds | How you get it |
|---|---|---|
| **Audit** page in the console (admins only) | Every call through the gateway: time (UTC), email, route, server, tool, model, status, tokens, routing label. Denied calls are included, with the model name the person asked for; model-list calls (`/v1/models`) are shown as "Model list". Requests with a wrong, revoked or expired key have no person: a second search lists them with time, path, status, the gateway's reason and the client address. A third search, **MCP app requests and approvals**, lists who asked for an MCP server, who approved or denied it, who approved changed tool definitions, and every refused attempt (for example a viewer who tried to approve, or a person who tried to approve their own request), with time, role, server, image or URL, comment and reason. | Search by email and dates, for requests with an invalid key, or for MCP app requests and approvals (email optional); **Export** to CSV. Needs `evidence.enabled: true`. |
| **ClickHouse** table `otel_logs` | The same records, as the source data. | Your own SQL queries. Retention is `evidence.retention` (default `8760h`, one year). The key ID of a call made with a personal key or a service account key is in `LogAttributes['ambit.key_id']`; `LogAttributes['ambit.identity']` is `service account`, `key`, `invalid key` or `jwt`. The Audit page and its CSV do not show the key ID. The records of MCP app requests and decisions have `LogAttributes['ambit.kind'] = 'app-approval'`; see [MCP app requests and approvals](#mcp-app-requests-and-approvals). The AccessRules in force are recorded as `LogAttributes['ambit.kind'] = 'access-snapshot'`; see [Access snapshots](#access-snapshots). |
| **ClickHouse** tables `ambit_llm_daily_v1`, `ambit_compress_daily_v1`, `ambit_classifier_daily_v1` | Daily counts behind the Savings page: requests and tokens per day, group, router and model; compressed tool result texts and bytes per day, group, server and type; router classifier calls and tokens per day, group and router. They hold no email and no content. Group names are the only identifying text. | Your own SQL queries. Retention is `evidence.summaryRetention` (default `26280h`, three years). They count from the day the console created them; there is no backfill. |
| **Usage** page (admins only) | Requests, tokens and estimated cost per group per day, for the last 30 days. | Console. |
| **Kubernetes objects** (`AccessRule`, `Model`, `Server`, `Budget`, `ModelRouter`, `Connection`) | Who may use what. With GitOps mode, every change is a pull request with its author, reviewer and time. | `kubectl get`, or your configuration repository. |
| **Settings → Keys** (admins only) | All personal API keys and service account keys, and the history of key changes: who created or revoked which key, settings changes, sign-ins that changed groups, and for service accounts `sa-create`, `sa-delete`, `sa-delete-key` and `exposed` (a key's end date lowered because the admin who issued it was offboarded) (newest 500). | Console. |
| **Settings → Service accounts** (admins only) | Each [service account](service-accounts.md): identity, group, description, who created it and when, its keys with who issued each one, and warnings (an issuer offboarded, a key not used for 30 days). The Audit page marks calls made by a service account. | Console. |
| **Settings → Offboarding** (admins only) | One record per person who left: trigger, sessions ended, gateway block, keys deleted, and each app vendor's answer to the revocation request. | Console. |
| **Servers** page | Requests to add an MCP server: who asked, who decided, the decision and the image digest. A person cannot approve their own request. | Console. The flow is described in [Ask for an MCP app and approve it](guides/request-an-app.md). The same steps are also audit records (above), which stay when a `ServerRequest` is deleted. |
| **Helm values** | The settings that matter to an auditor: token issuers, TLS, `bootstrapAdmin.enabled`, `apiKeys.*`, `serviceAccounts.*`, `evidence.*`, `limits.*`, `protection.enabled`. | `helm get values ambit -n ambit-system`, or your values file in Git. |

The Audit and Usage pages and the ClickHouse records exist only with `evidence.enabled: true`. The default is `false`.

### MCP app requests and approvals

The console writes one record for each step of an MCP app (an MCP server) request, through the same collector as the gateway's records, into `otel_logs`, with the same retention (`evidence.retention`). It also writes a Kubernetes Event on the `ServerRequest` (or the `Server`), but Kubernetes keeps Events for one hour by default, so use the records in ClickHouse as the evidence.

| `ambit.app.action` | When |
|---|---|
| `requested` | A person sent a request. |
| `approved` | An approver approved it. In GitOps mode, written when the console sees the pull request merged: the time is the merge time, and the person is the approver who opened the pull request. |
| `pull-request-opened` | GitOps mode: an approver approved, and the console opened the pull request. |
| `denied` | An approver denied it. |
| `tools-approved` | An admin approved changed tool definitions (tool pinning, `protection.enabled`). |
| `approve-refused`, `deny-refused`, `tools-approve-refused` | The console refused the attempt. The reason says why: for example `role viewer may not decide on server requests`, `own request`, `already approved`, `the pull request is still open`, `no such request`. |

Fields (`LogAttributes`): `ambit.kind` (`app-approval`), `ambit.app.action`, `ambit.actor` (the email from the console sign-in), `ambit.actor.role` (`admin`, `approver` or `viewer`; empty for a GitOps approval seen at the merge), `mcp.target` (the server name), `ambit.app.request` (the `ServerRequest` name), `ambit.app.requested_by`, `ambit.app.image` or `ambit.app.url`, `ambit.app.comment` (the reason of the request, or the comment of the decision), `ambit.app.pull_request`, `ambit.app.tools` (a JSON array, for `tools-approved`) and `ambit.app.refused_reason`. A refused attempt does not record the comment the person typed. These records have no `jwt.email`, so the Audit search for one person's calls, the Usage page and the Savings page do not count them as calls.

Example query: who approved which MCP servers in March 2026.

```sql
SELECT Timestamp, LogAttributes['mcp.target'] AS server, LogAttributes['ambit.actor'] AS approved_by,
       LogAttributes['ambit.app.requested_by'] AS requested_by, LogAttributes['ambit.app.image'] AS image
FROM otel_logs
WHERE LogAttributes['ambit.kind'] = 'app-approval' AND LogAttributes['ambit.app.action'] = 'approved'
  AND Timestamp >= '2026-03-01' AND Timestamp < '2026-04-01'
ORDER BY Timestamp
```

If the console cannot send a record to the collector, the step still happens, the console logs an error, and the alert `AmbitConsoleAuditSendFailed` fires (with `alerts.enabled`). The `ServerRequest` object still holds the request and its decision.

### Access snapshots

With `evidence.enabled`, the controller writes a record of every AccessRule in force into `otel_logs`, through the same collector, with the same retention (`evidence.retention`). With these records you can see who was allowed to use what on a past date, also after the rules changed. A record is written:

- when a change of the rules or of the access mode is active in the gateway;
- when only an AccessRule's text changes in a way the gateway does not see, for example an argument limit (the guard reads it directly);
- when the controller starts;
- once a day at 00:05 UTC, with the same content, so each day inside the retention has a record even when nothing changes.

Fields: `ambit.kind` (`access-snapshot`), `ambit.rulesVersion` (the same value as `ambit.rulesVersion` on the gateway's records judged by these rules), `ambit.rulesHash` (SHA-256 of the body), `ambit.snapshot.accessMode` (`enforce` or `audit`). The body (`Body`) is a JSON array of every AccessRule, `{"name": ..., "spec": ...}`, sorted by name. The only personal data is the emails in `users`, which are in the AccessRules already. These records have no `jwt.email`, so the Audit, Usage and Savings pages do not count them.

The rules in force at the end of a day D are in the newest record with `Timestamp` before the end of D:

```sql
SELECT Timestamp, LogAttributes['ambit.rulesVersion'] AS rules_version, LogAttributes['ambit.snapshot.accessMode'] AS mode, Body
FROM otel_logs
WHERE LogAttributes['ambit.kind'] = 'access-snapshot' AND Timestamp < '2026-10-01'
ORDER BY Timestamp DESC
LIMIT 1
```

Records start at the upgrade to 0.5.0; there are none for earlier dates. Sending is best effort: when the collector cannot be reached, or the AccessRules together are larger than 1 MiB, the record is dropped, `ambit_controller_record_dropped_total` goes up and the alert `AmbitControllerRecordsDropped` fires (with `alerts.enabled`). The rules still apply. A missing change shows as two records in a row with different `ambit.rulesHash` values.

**Removing an app is not recorded here.** The console has no action that removes an approved MCP server. A `Server` is removed through your configuration repository in GitOps mode, where the Git history records it, or with `kubectl`, where the Kubernetes audit log of your cluster records it (if your cluster has audit logging on).

**Audit-only mode.** With `access.mode: audit`, the records are of requests that were allowed. Each record carries the verdict of the access rules in force when it was served (`ambit.wouldRefuse` for a model request, `ambit.wouldRefuseTool` for an MCP tool call: `allow`, `refuse:model`, `refuse:router`, `refuse:tool`, `other-model` or `n/a`), the mode (`ambit.accessMode`) and the version of those rules (`ambit.rulesVersion`). The verdict is not recomputed later, so a rule written afterwards does not change an older record. A request refused for another reason (sign-in, a budget, an unpriced model) has its real status, 401, 403 or 429. See [Turn Ambit on without blocking anyone](guides/audit-only.md).

## SOC 2

SOC 2 reports use the AICPA Trust Services Criteria (the 2017 criteria, with the points of focus revised in 2022). The table covers the criteria where Ambit has something to show. The short labels next to the IDs are descriptions in this page's own words, not AICPA titles. "Your part" is what Ambit cannot do for you.

| Criterion | What an auditor asks | What Ambit provides | Evidence today | Your part |
|---|---|---|---|---|
| **CC6.1** Logical access security | Is access to information assets limited by security software and architecture? | Every request to a model or MCP server passes one gateway that checks a company token or a personal API key. Nothing is allowed by default. Provider credentials stay in Kubernetes Secrets and never reach the client. Admission policies protect the key Secrets. | `llm.jwt` and `mcp.jwt` and `gateway.tls` in the Helm values; `AccessRule` objects | Network, cluster and identity provider security. Ambit protects only the AI path. |
| **CC6.2** Registering users, issuing and removing credentials | Are users registered and approved before access, and is access removed when it is no longer needed? | People sign in through your identity provider. Personal keys have an end date (30, 90 or 365 days) and stop after 30 days (default) without a console sign-in. When the identity provider deactivates a person (SCIM), Ambit ends their sessions, blocks and deletes their keys, and revokes stored app grants where the vendor allows it. | Settings → Offboarding records; Settings → Keys history; your identity provider's own records | Run the joiner and leaver process in your identity provider. Check the vendor answers in each offboarding record. |
| **CC6.3** Role-based access, least privilege, separation of duties | Is access granted by role and limited to what is needed? | Access rules grant models, MCP servers and single tools to identity-provider groups. A rule can limit the values of tool arguments. Console roles come from groups: `ambit-admins`, `ambit-approvers`, and view-only for everyone else. A person cannot approve their own server request. Each request, approval and denial, and each refused attempt (a viewer who tries to approve, a self-approval), is an audit record with the person, the role and the time. | `AccessRule` objects; Access rules and Servers pages; Audit page, search **MCP app requests and approvals** | Define the groups and review who is in them. Review of access is your process; Ambit does not remind you to do it. |
| **CC6.6** Protection against threats from outside the system boundary | Is the system protected from outside threats? | The gateway refuses requests without valid credentials. Pods of MCP servers that ToolHive runs are reachable only by the gateway. Daily budgets cap the use of a stolen credential. | `gateway.tls` and the Audit page (refused calls) | Firewall, network policy and exposure of the gateway to the internet. Ambit is not a web application firewall. |
| **CC6.7** Restricting transmission, movement and removal of information | Is information limited to authorised people and protected in transit? | HTTPS on the gateway (`gateway.tls`). On the MCP path, personal data in tool results is masked (card numbers, Korean resident registration numbers, US Social Security numbers and emails; names, addresses and more with the optional Presidio component, see [Masking](concepts.md#masking)). With `protection.enabled`, tool pinning refuses a tool whose definition changed. | `protection.enabled` and `gateway.tls` in the Helm values | On the model path, prompts are masked only by the [prompt guard](concepts.md#prompt-guard) in mode `mask`, and answers are not masked. See [Limits](#limits). |
| **CC7.2** Monitoring for anomalies | Are system components monitored for unusual activity? | Every call is recorded. The Usage page shows spend and requests per group per day. Prometheus alerts cover restarts, memory, a budget store that is down, audit records that fail to reach ClickHouse, and records of MCP app approvals that the console could not send. | Audit and Usage pages; `alerts.enabled`; ClickHouse queries | Ambit does not detect anomalies by itself. You query the records or send them to your monitoring system. |
| **CC7.3** Evaluating security events | Are possible security events evaluated? | The Audit page finds all calls of one person in a date range, counts refused and failed calls, and exports them to CSV. | Audit page and CSV export | Your incident process. Ambit supplies the records for it. |
| **CC8.1** Change management | Are changes to infrastructure and software authorised, tested and approved? | In GitOps mode, the console opens a pull request in your configuration repository for every change, and Argo CD applies what you merge. With `protection.enabled`, a tool whose definition changed stays hidden until an admin approves it again. Each new MCP server and each approval of changed tool definitions is an audit record: who asked, who approved, when, and which image or tools. | Pull request history in your configuration repository; Audit page, search **MCP app requests and approvals** | Your review and branch rules. In direct mode the console changes the cluster at once, with no pull request. |

CC6.8 (malicious software) and the availability, confidentiality and privacy categories are not mapped here.

## ISO/IEC 27001:2022

The controls below are in Annex A of ISO/IEC 27001:2022. Their titles and numbers are the same in ISO/IEC 27002:2022, which explains each control.

| Control | What Ambit provides | Evidence today | Your part |
|---|---|---|---|
| **5.15** Access control | Nothing is allowed by default. An `AccessRule` grants a group access to named models, MCP servers and tools. | `AccessRule` objects; Access rules page | Your access control policy, and the decision of who gets which group. |
| **5.16** Identity management | Ambit keeps no user directory. Identities come from your identity provider (email and groups in the token). SCIM deactivation ends a person's access in seconds. In one test, the first refusal came a median of about 350 ms after the revocation (see [Personal API keys](api-keys.md#measured)). | Helm values for the token issuer; Settings → Offboarding records | The identity lifecycle itself, including groups. |
| **5.17** Authentication information | A personal API key is shown once. Ambit stores only a SHA-256 hash. Keys have an end date and an inactivity limit. `apiKeys.enabled: false` refuses all keys at once. The bootstrap admin can be removed. | `apiKeys.*` and `bootstrapAdmin.enabled` in the Helm values; Settings → Keys | Passwords and multi-factor sign-in belong to your identity provider. |
| **5.18** Access rights | Rights are given through groups and rules, and they end when a person is offboarded or a key expires. Admins see every key and can revoke any of them. | Access rules page; Settings → Keys and its history | The periodic review of access rights, and its record. |
| **8.2** Privileged access rights | The console admin role is the group `ambit-admins`; the approver role is `ambit-approvers`. Console sessions last at most `console.sessionHours` (default 1). Key changes are logged. Each use of the approver and admin rights on MCP apps (approve, deny, approve changed tools) is an audit record, and so is each refused attempt. | Helm values; Settings → Keys history; Audit page, search **MCP app requests and approvals** | Who is in these groups, and how you control their use. Roles are group-based only; there are no custom roles. |
| **8.12** Data leakage prevention | On the MCP path, with `protection.enabled`: masking of personal data in tool results (card numbers, Korean resident registration numbers, US Social Security numbers and emails; more with the optional Presidio component, see [Masking](concepts.md#masking)), limits on tool arguments, and tool pinning. Result compression (`Server.spec.resultCompression`) is a cost control, not a security control; it runs before masking, so the masks still apply to what the client receives. While Presidio is set, JSON is not compressed, because removing its spaces lowered the share of names Presidio finds from 93.2% to 10.0% in a test (see [Compressing tool results](concepts.md#compressing-tool-results)). | `protection.enabled` in the Helm values; `llm.promptGuard` for prompts | On the model path, prompts are checked and masked only with the [prompt guard](concepts.md#prompt-guard) (patterns you set, off by default); answers are not checked. See [Limits](#limits). |
| **8.15** Logging | Every call is recorded, denied calls included. MCP app requests, approvals, denials and refused attempts are recorded too. Records go through the OpenTelemetry Collector to ClickHouse and are kept for `evidence.retention`. An alert fires when records fail to arrive. | Audit page; `otel_logs` table; `AmbitAuditLogsDropped` and `AmbitConsoleAuditSendFailed` alerts | Protect and back up ClickHouse, and limit who can read or change it. Ambit does not sign the log. |
| **8.16** Monitoring activities | The Audit and Usage pages, Prometheus alerts, and daily budgets that stop unusual spending. | Audit and Usage pages; [Alerts](alerts.md) | Reviewing the records and acting on them. |

## EU AI Act

Regulation (EU) 2024/1689 sets record-keeping duties for **high-risk** AI systems. Ambit records the calls that pass through it. It does not decide whether your system is high-risk, and it does not make a system compliant by itself.

The duties for high-risk systems apply from 2 December 2027 for systems listed in Annex III and from 2 August 2028 for systems that are safety components of products under Annex I. Regulation (EU) 2026/1744 (the Digital Omnibus on AI, in force since 27 July 2026) moved them from 2 August 2026. These dates come from summaries of the regulation, not from the Official Journal text. Confirm the dates and the article texts below on EUR-Lex before you rely on them.

| Provision | What it says | What Ambit provides |
|---|---|---|
| **Article 12** Record-keeping | A high-risk AI system must technically allow the automatic recording of events (logs) over its lifetime. | Ambit records each call to a model or MCP server with time, caller, model or tool, status and token counts. Whether these events are the ones Article 12 requires for your system is for you and your provider to decide. |
| **Article 19** Automatically generated logs | The provider of a high-risk system keeps the logs that its system generates, to the extent the logs are under its control, for a period suited to the purpose of the system and at least six months, unless other Union or national law says otherwise. | If you are the provider, Ambit's `evidence.retention` sets how long the records stay in ClickHouse. The default is one year. |
| **Article 26(6)** Obligations of deployers | A deployer keeps the logs automatically generated by the high-risk system, to the extent the logs are under its control, for a period suited to the purpose of the system and at least six months, unless other Union or national law says otherwise. | If you are the deployer, keep `evidence.retention` at six months or more, and check data protection law for the maximum. |

Ambit records metadata and token counts. It does not store the text of prompts or answers. If your system needs the content, you must record it elsewhere.

## Limits

These limits come from the current release. Tell your auditor about the ones that matter to a control.

- **Prompts are checked only by patterns you set, and answers are not checked.** Masking and argument limits work on MCP tool results and tool calls, with `protection.enabled`. On the model path, the [prompt guard](concepts.md#prompt-guard) (`llm.promptGuard`, off by default) checks prompts against built-in patterns, your own regular expressions and an optional webhook in your cluster, and records, refuses or masks what matches. It is a pattern filter, not a prompt-injection detector: it finds only the text its patterns describe. It does not check answers or embeddings. Vendor guardrails (OpenAI Moderation, Amazon Bedrock Guardrails, Google Model Armor) are not supported yet.
- **With `shorten`, the model can receive a part of a tool result.** `Server.spec.resultCompression.mode: shorten` leaves out repeated log lines, passing tests, YAML comments and `managedFields`, and marks each cut in the text. Ambit does not store the original, so nobody can compare the original with what the client received (this is also true for masking). `lossless` keeps all information. This is a change to a tool result on the MCP path, where masking already works. It is not a change to a prompt on the model path: the client puts the result in its own next request, and Ambit does not change that request (except for the masks of the [prompt guard](concepts.md#prompt-guard) in mode `mask`). The line that `shorten` adds to a result is fixed Ambit text, not text added to a user's prompt.
- **Masking covers tool results only.** On the MCP path, the guard masks the result of a tool call. A JSON-RPC error, a log or progress notification, or a sampling or elicitation request that a server sends during the call reaches the client unmasked, because the gateway does not send these messages to the guard (see [Masking](concepts.md#masking)). The audit log does not store the text of an MCP error: the record keeps `mcp.error.code` and drops `mcp.error.message`, also in the gateway's own log lines.
- **Compression leaves no per-call record.** The guard counts compressed bytes per server in Prometheus metrics (they reset when the pod restarts) and writes one log line per compressed text with sizes, server and mode (the cluster's log retention applies). Neither names the caller, and neither links to a row on the Audit page. They show that a server's results are shortened and by how much. They do not show which call was shortened or what one client received. Neither holds the content of any result. If an auditor needs a record per call, that is not available yet.
- **Embeddings are counted but not charged.** The request count includes them; the dollar cost does not (see [Budgets](budgets.md)). Dollar amounts are estimates from list prices.
- **Budgets cover model requests only**, not MCP calls, and a limit can be exceeded by requests that are already running.
- **The audit log is not tamper-evident.** Ambit does not sign records, and an export to a SIEM is not documented. Anyone with write access to ClickHouse can change the records.
- **Evidence is gathered by hand.** The Audit page exports CSV, and everything else is a console page or a query. A ready-made evidence export is planned, not shipped.
- **Group changes reach the console at the next sign-in**, so a removal from a group can take up to `console.sessionHours` to apply. Personal API keys keep the groups of the owner's last console sign-in for up to `apiKeys.inactivityDays` (default 30). To remove access at once, revoke the keys or offboard the person. Offboarding through SCIM acts at once.
- **Offboarding cannot revoke every app grant.** Microsoft 365 and Atlassian offer no way to revoke a user's grant to a third-party app. Revocation formats for GitHub, Slack and Notion are not yet tested against the real providers. See [Offboarding](offboarding.md).
- **One console replica**, and the chart's Valkey keeps no data after a restart (budget counters return to zero).
- **Not yet tested with real accounts:** Azure OpenAI, Amazon Bedrock and Vertex AI. There is no 24-hour soak test yet.
- **Alpha.** The API is `ambitproject.dev/v1alpha1` and can change between releases.

## Supply chain

For every release, the workflow `.github/workflows/release.yml` builds the console image `ghcr.io/craftsman-apps/ambit-console` for `linux/amd64` and `linux/arm64`, and publishes with it:

- an **SBOM** (software bill of materials, in SPDX format), and
- **build provenance** (a SLSA record of how and where the image was built, at `mode=max`),

both stored with the image in the registry. You can read them:

```sh
docker buildx imagetools inspect ghcr.io/craftsman-apps/ambit-console:0.1.0 --format '{{ json .SBOM }}'
docker buildx imagetools inspect ghcr.io/craftsman-apps/ambit-console:0.1.0 --format '{{ json .Provenance }}'
```

From 0.4.0 the image is also **signed** with [cosign](https://docs.sigstore.dev/cosign/) keyless signing: the
signature is bound to the release workflow's GitHub identity and recorded in Sigstore's public transparency log. To
verify a tag (cosign 2.0 or newer):

```sh
cosign verify ghcr.io/craftsman-apps/ambit-console:0.4.0 \
  --certificate-identity-regexp '^https://github.com/craftsman-apps/ambit/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The command fails for an image that was not signed by that workflow on a `v*` tag. Releases before 0.4.0 are not signed.

Image labels give the source repository, licence (Apache-2.0), version and commit. The workflow's actions are pinned to commit hashes, and Dependabot proposes updates for Go modules, npm, base images and actions.

**Not yet shipped:** the Helm charts are published to the registry without an SBOM or signature. Third-party images that the chart installs (agentgateway, ToolHive, Dex, ClickHouse and others) come from their own projects, with their own release practices.

## Sources

The full texts of the SOC 2 criteria and the ISO controls are not free to read. This page names each control by its published number and describes it in its own words, so confirm the exact wording in the documents below before you quote it to an auditor.

- AICPA, *2017 Trust Services Criteria for Security, Availability, Processing Integrity, Confidentiality, and Privacy (With Revised Points of Focus — 2022)*, TSP Section 100: criteria CC6.1, CC6.2, CC6.3, CC6.6, CC6.7, CC7.2, CC7.3 and CC8.1. <https://www.aicpa-cima.com/resources/download/2017-trust-services-criteria-with-revised-points-of-focus-2022>
- ISO/IEC 27001:2022, Annex A, and ISO/IEC 27002:2022 (controls 5.15, 5.16, 5.17, 5.18, 8.2, 8.12, 8.15, 8.16). <https://www.iso.org/standard/75652.html>
- Regulation (EU) 2024/1689 (Artificial Intelligence Act), Articles 12, 19 and 26(6). <https://eur-lex.europa.eu/eli/reg/2024/1689/oj>
- Regulation (EU) 2026/1744 (Digital Omnibus on AI), which amends Article 113 and sets the application dates above. EUR-Lex: <https://eur-lex.europa.eu/eli/reg/2026/1744/oj>. Summary with the amended text: <https://artificialintelligenceact.eu/ai-act-explorer/digital-omnibus/>
- Ambit: [Concepts](concepts.md), [Install on your cluster](install.md), [Personal API keys](api-keys.md), [Offboarding](offboarding.md), [Budgets](budgets.md), [Alerts](alerts.md), and `.github/workflows/release.yml`.
