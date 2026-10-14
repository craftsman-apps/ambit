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
| **Audit** page in the console (admins only) | Every call through the gateway: time (UTC), email, route, server, tool, model, status, tokens, routing label. Denied calls are included. | Search by email and dates; **Export** to CSV. Needs `evidence.enabled: true`. |
| **ClickHouse** table `otel_logs` | The same records, as the source data. | Your own SQL queries. Retention is `evidence.retention` (default `8760h`, one year). The key ID of a call made with a personal key is in `LogAttributes['ambit.key_id']`; the Audit page and its CSV do not show it. |
| **Usage** page (admins only) | Requests, tokens and estimated cost per group per day, for the last 30 days. | Console. |
| **Kubernetes objects** (`AccessRule`, `Model`, `Server`, `Budget`, `ModelRouter`, `Connection`) | Who may use what. With GitOps mode, every change is a pull request with its author, reviewer and time. | `kubectl get`, or your configuration repository. |
| **Settings → Keys** (admins only) | All personal API keys, and the history of key changes: who created or revoked which key, settings changes, sign-ins that changed groups (newest 500). | Console. |
| **Settings → Offboarding** (admins only) | One record per person who left: trigger, sessions ended, gateway block, keys deleted, and each app vendor's answer to the revocation request. | Console. |
| **Servers** page | Requests to add an MCP server: who asked, who decided, the decision and the image digest. A person cannot approve their own request. | Console. This flow is in the console but not yet described in these docs; see `internal/console/servers.go` in the repository. |
| **Helm values** | The settings that matter to an auditor: token issuers, TLS, `bootstrapAdmin.enabled`, `apiKeys.*`, `evidence.*`, `limits.*`, `protection.enabled`. | `helm get values ambit -n ambit-system`, or your values file in Git. |

The Audit and Usage pages and the ClickHouse records exist only with `evidence.enabled: true`. The default is `false`.

## SOC 2

SOC 2 reports use the AICPA Trust Services Criteria (the 2017 criteria, with the points of focus revised in 2022). The table covers the criteria where Ambit has something to show. The short labels next to the IDs are descriptions in this page's own words, not AICPA titles. "Your part" is what Ambit cannot do for you.

| Criterion | What an auditor asks | What Ambit provides | Evidence today | Your part |
|---|---|---|---|---|
| **CC6.1** Logical access security | Is access to information assets limited by security software and architecture? | Every request to a model or MCP server passes one gateway that checks a company token or a personal API key. Nothing is allowed by default. Provider credentials stay in Kubernetes Secrets and never reach the client. Admission policies protect the key Secrets. | `llm.jwt` and `mcp.jwt` and `gateway.tls` in the Helm values; `AccessRule` objects | Network, cluster and identity provider security. Ambit protects only the AI path. |
| **CC6.2** Registering users, issuing and removing credentials | Are users registered and approved before access, and is access removed when it is no longer needed? | People sign in through your identity provider. Personal keys have an end date (30, 90 or 365 days) and stop after 30 days (default) without a console sign-in. When the identity provider deactivates a person (SCIM), Ambit ends their sessions, blocks and deletes their keys, and revokes stored app grants where the vendor allows it. | Settings → Offboarding records; Settings → Keys history; your identity provider's own records | Run the joiner and leaver process in your identity provider. Check the vendor answers in each offboarding record. |
| **CC6.3** Role-based access, least privilege, separation of duties | Is access granted by role and limited to what is needed? | Access rules grant models, MCP servers and single tools to identity-provider groups. A rule can limit the values of tool arguments. Console roles come from groups: `ambit-admins`, `ambit-approvers`, and view-only for everyone else. A person cannot approve their own server request. | `AccessRule` objects; Access rules and Servers pages | Define the groups and review who is in them. Review of access is your process; Ambit does not remind you to do it. |
| **CC6.6** Protection against threats from outside the system boundary | Is the system protected from outside threats? | The gateway refuses requests without valid credentials. Pods of MCP servers that ToolHive runs are reachable only by the gateway. Daily budgets cap the use of a stolen credential. | `gateway.tls` and the Audit page (refused calls) | Firewall, network policy and exposure of the gateway to the internet. Ambit is not a web application firewall. |
| **CC6.7** Restricting transmission, movement and removal of information | Is information limited to authorised people and protected in transit? | HTTPS on the gateway (`gateway.tls`). On the MCP path, personal data in tool results is masked (card numbers, Korean resident registration numbers, US Social Security numbers and emails; names, addresses and more with the optional Presidio component). With `protection.enabled`, tool pinning refuses a tool whose definition changed. | `protection.enabled` and `gateway.tls` in the Helm values | Prompts and answers on the model path are not masked. See [Limits](#limits). |
| **CC7.2** Monitoring for anomalies | Are system components monitored for unusual activity? | Every call is recorded. The Usage page shows spend and requests per group per day. Prometheus alerts cover restarts, memory, a budget store that is down, and audit records that fail to reach ClickHouse. | Audit and Usage pages; `alerts.enabled`; ClickHouse queries | Ambit does not detect anomalies by itself. You query the records or send them to your monitoring system. |
| **CC7.3** Evaluating security events | Are possible security events evaluated? | The Audit page finds all calls of one person in a date range, counts refused and failed calls, and exports them to CSV. | Audit page and CSV export | Your incident process. Ambit supplies the records for it. |
| **CC8.1** Change management | Are changes to infrastructure and software authorised, tested and approved? | In GitOps mode, the console opens a pull request in your configuration repository for every change, and Argo CD applies what you merge. With `protection.enabled`, a tool whose definition changed stays hidden until an admin approves it again. | Pull request history in your configuration repository | Your review and branch rules. In direct mode the console changes the cluster at once, with no pull request. |

CC6.8 (malicious software) and the availability, confidentiality and privacy categories are not mapped here.

## ISO/IEC 27001:2022

The controls below are in Annex A of ISO/IEC 27001:2022. Their titles and numbers are the same in ISO/IEC 27002:2022, which explains each control.

| Control | What Ambit provides | Evidence today | Your part |
|---|---|---|---|
| **5.15** Access control | Nothing is allowed by default. An `AccessRule` grants a group access to named models, MCP servers and tools. | `AccessRule` objects; Access rules page | Your access control policy, and the decision of who gets which group. |
| **5.16** Identity management | Ambit keeps no user directory. Identities come from your identity provider (email and groups in the token). SCIM deactivation ends a person's access in seconds. In one test, the first refusal came a median of about 350 ms after the revocation (see [Personal API keys](api-keys.md#measured)). | Helm values for the token issuer; Settings → Offboarding records | The identity lifecycle itself, including groups. |
| **5.17** Authentication information | A personal API key is shown once. Ambit stores only a SHA-256 hash. Keys have an end date and an inactivity limit. `apiKeys.enabled: false` refuses all keys at once. The bootstrap admin can be removed. | `apiKeys.*` and `bootstrapAdmin.enabled` in the Helm values; Settings → Keys | Passwords and multi-factor sign-in belong to your identity provider. |
| **5.18** Access rights | Rights are given through groups and rules, and they end when a person is offboarded or a key expires. Admins see every key and can revoke any of them. | Access rules page; Settings → Keys and its history | The periodic review of access rights, and its record. |
| **8.2** Privileged access rights | The console admin role is the group `ambit-admins`; the approver role is `ambit-approvers`. Console sessions last at most `console.sessionHours` (default 1). Key changes are logged. | Helm values; Settings → Keys history | Who is in these groups, and how you control their use. Roles are group-based only; there are no custom roles. |
| **8.12** Data leakage prevention | On the MCP path, with `protection.enabled`: masking of personal data in tool results (card numbers, Korean resident registration numbers, US Social Security numbers and emails; more with the optional Presidio component), limits on tool arguments, and tool pinning. | `protection.enabled` in the Helm values | No masking or content filter on model prompts and answers. See [Limits](#limits). |
| **8.15** Logging | Every call is recorded, denied calls included. Records go through the OpenTelemetry Collector to ClickHouse and are kept for `evidence.retention`. An alert fires when records fail to arrive. | Audit page; `otel_logs` table; `AmbitAuditLogsDropped` alert | Protect and back up ClickHouse, and limit who can read or change it. Ambit does not sign the log. |
| **8.16** Monitoring activities | The Audit and Usage pages, Prometheus alerts, and daily budgets that stop unusual spending. | Audit and Usage pages; [Alerts](alerts.md) | Reviewing the records and acting on them. |

## EU AI Act

Regulation (EU) 2024/1689 sets record-keeping duties for **high-risk** AI systems. Ambit records the calls that pass through it. It does not decide whether your system is high-risk, and it does not make a system compliant by itself.

The duties for high-risk systems apply from 2 December 2027 for systems listed in Annex III and from 2 August 2028 for systems that are safety components of products under Annex I. Regulation (EU) 2026/1744 (the Digital Omnibus on AI, in force since 27 July 2026) moved them from the earlier date of 2 August 2026. These dates come from secondary sources that cite the regulation, and I could not read the Official Journal text. Confirm the dates and the article texts below on EUR-Lex before you rely on them.

| Provision | What it says | What Ambit provides |
|---|---|---|
| **Article 12** Record-keeping | A high-risk AI system must technically allow the automatic recording of events (logs) over its lifetime. | Ambit records each call to a model or MCP server with time, caller, model or tool, status and token counts. Whether these events are the ones Article 12 requires for your system is for you and your provider to decide. |
| **Article 19** Automatically generated logs | The provider of a high-risk system keeps the logs that its system generates, to the extent the logs are under its control, for a period suited to the purpose of the system and at least six months, unless other Union or national law says otherwise. | If you are the provider, Ambit's `evidence.retention` sets how long the records stay in ClickHouse. The default is one year. |
| **Article 26(6)** Obligations of deployers | A deployer keeps the logs automatically generated by the high-risk system, to the extent the logs are under its control, for a period suited to the purpose of the system and at least six months, unless other Union or national law says otherwise. | If you are the deployer, keep `evidence.retention` at six months or more, and check data protection law for the maximum. |

Ambit records metadata and token counts. It does not store the text of prompts or answers. If your system needs the content, you must record it elsewhere.

## Limits

These limits come from the current release. Tell your auditor about the ones that matter to a control.

- **Model refusals do not name the caller.** When the model router refuses a request (status 403 or 404), Ambit records the refusal but not who made it. The cause is a bug in agentgateway v1.6.0. See [Known limitations](index.md#known-limitations). This affects the statement "every call is recorded with its caller" (8.15, CC7.3).
- **No prompt or answer content, no masking on the model path.** Masking and argument limits work on MCP tool results and tool calls only, and only with `protection.enabled`. Ambit has no prompt-injection filter and does not support provider guardrails.
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

Image labels give the source repository, licence (Apache-2.0), version and commit. The workflow's actions are pinned to commit hashes, and Dependabot proposes updates for Go modules, npm, base images and actions.

**Not yet shipped:** image signatures. They are planned. The Helm charts are published to the registry without an SBOM or signature. Third-party images that the chart installs (agentgateway, ToolHive, Dex, ClickHouse and others) come from their own projects, with their own release practices.

## Sources

The full texts of the SOC 2 criteria and the ISO controls are not free to read. This page names each control by its published number and describes it in its own words, so confirm the exact wording in the documents below before you quote it to an auditor.

- AICPA, *2017 Trust Services Criteria for Security, Availability, Processing Integrity, Confidentiality, and Privacy (With Revised Points of Focus — 2022)*, TSP Section 100: criteria CC6.1, CC6.2, CC6.3, CC6.6, CC6.7, CC7.2, CC7.3 and CC8.1. <https://www.aicpa-cima.com/resources/download/2017-trust-services-criteria-with-revised-points-of-focus-2022>
- ISO/IEC 27001:2022, Annex A, and ISO/IEC 27002:2022 (controls 5.15, 5.16, 5.17, 5.18, 8.2, 8.12, 8.15, 8.16). <https://www.iso.org/standard/75652.html>
- Regulation (EU) 2024/1689 (Artificial Intelligence Act), Articles 12, 19 and 26(6). <https://eur-lex.europa.eu/eli/reg/2024/1689/oj>
- Regulation (EU) 2026/1744 (Digital Omnibus on AI), which amends Article 113 and sets the application dates above. EUR-Lex: <https://eur-lex.europa.eu/eli/reg/2026/1744/oj>. Summary with the amended text: <https://artificialintelligenceact.eu/ai-act-explorer/digital-omnibus/>
- Ambit: [Concepts](concepts.md), [Install on your cluster](install.md), [Personal API keys](api-keys.md), [Offboarding](offboarding.md), [Budgets](budgets.md), [Alerts](alerts.md), and `.github/workflows/release.yml`.
