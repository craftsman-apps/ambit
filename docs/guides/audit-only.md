---
title: Turn Ambit on without blocking anyone
description: Install with access.mode audit so every signed-in person can work, see which requests your access rules would refuse, write the rules from that list, and then switch to enforcing.
---

# Turn Ambit on without blocking anyone

**What you get:** a way to start Ambit before your access rules are complete. In audit-only mode every signed-in request to a model or an MCP tool goes through. Each request's audit record says whether the access rules in force at that moment would have refused it. You install, watch for a week, write rules until the "would be refused" count is near zero, and then switch to enforcing.

Ambit refuses everything that no access rule grants. On the first day that blocks every person. Audit-only mode is like a door lock in "log only" mode: the door opens for everyone with a badge, and the log line written at that moment says whether the lock, as set at that moment, would have refused that badge.

## What changes, and what does not

| | In audit-only mode |
|---|---|
| Model access (`AccessRule` `models`) and ModelRouter lines | **Allowed**, and marked in the audit record |
| MCP `tools/call` (`AccessRule` `servers` and `tools`) | **Allowed**, and marked in the audit record |
| MCP `tools/list` | **Not filtered by access rules: every signed-in person sees every tool of every server.** Not marked |
| A `ModelRouter` of type LLM with category routes | **Does not classify a person that the router's groups do not grant.** That person is served by the default model, or by a route without a category, and the record says `refuse:router`. After a grant, the person gets the routed model |
| Sign-in (a valid token or key), the LLM path allowlist | Still enforced |
| Offboarding | Still enforced |
| Budgets, and the refusal of an unpriced model for a group with a dollar limit | Still enforced |
| Argument limits and tool approval (tool pinning) | Still enforced |

Do not use audit-only mode while some tool must never be called by some people. Set argument limits or approval pins on that tool first: those stay enforced.

The mode is one Helm value for the whole cluster. Only a person who can run `helm upgrade` (or change the values file in Git) can switch it. A console admin or a rule author cannot.

## Before you start

You need an installed Ambit (see [Install on your cluster](../install.md)) with `evidence.enabled: true`. Without the audit store, audit-only mode would allow everything and record nothing, so Helm refuses the value `audit` when `evidence.enabled` is `false`.

## 1. Turn audit-only mode on

Add the value and upgrade:

```yaml
access:
  mode: audit   # enforce (the default) or audit
```

```sh
helm upgrade ambit oci://ghcr.io/craftsman-apps/charts/ambit --version <version> -n ambit-system -f values.yaml --wait
```

The controller changes the gateway in two steps, and waits for agentgateway to accept each step before it starts the next one:

1. It adds the "would be refused" fields to the access log.
2. It sets every authorization to "allow".

So no request is allowed without a mark. This takes from a few seconds to a minute (the controller restarts and elects a leader first). **A switch is complete only when the console banner says so:** while agentgateway has not applied a step, admins see "Mode switch to audit is not complete" with the reason, and the published mode (the ConfigMap `ambit-access-mode`) keeps the old value. If a step does not complete within 120 seconds (`access.activationTimeout`), the switch stops at that step, and every AccessRule shows `Ready=False` with the reason `ModeSwitchStalled`. The controller tries again, and a stopped switch never leaves a request allowed without a mark.

When it is complete, every page of the console shows a warning banner:

> **Audit-only mode since 2026-10-15 (today).** Ambit lets every signed-in request through, and every tool is listed to everyone. Access rules are recorded, not enforced.

Every AccessRule's `Ready` message ends with "audit-only mode: recorded, not enforced". The reason stays `Compiled`, so a GitOps health check does not turn red.

Check it from the command line:

```sh
export AMBIT_TOKEN=<a gateway token>
ambit check --production --deep
```

`P11` shows a warning, "access.mode is audit for 0 days", and with `--deep` it also confirms that a test call's record reached the audit store. If the record does not reach the store, `P11` fails: audit-only mode then allows everything and leaves no evidence.

## 2. Watch

Open **Evidence → Audit** in the console. While audit-only mode is on, and for 30 days of audit-mode records after it, the top of the page shows a summary:

> **Requests from 2026-10-08 10:00 to 2026-10-15 10:01 (UTC): 1,240 requests from 18 people would have been refused.** Newest record: 2026-10-15 09:58. Each mark was computed when the request was served, with the rules in force at that time.

Under it is a table of the group, the model or tool, the number of requests and the number of people. A person in two groups counts in both rows. There is no "by rule" column: a refusal means that no rule granted the request, so there is no rule to name.

Each record carries its verdict from the moment it was served. If you write a rule today, older records keep their old mark. Tick **Only since the current rules** to count only the records judged by the rules that are in force now (each set of rules has a version, shown next to the box). When that count is 0 for at least 7 days, the page says "Switching to enforcing now would refuse nothing seen since …".

The page sorts every request of audit mode into one of these outcomes, and never mixes them:

| Outcome | What it means | Counted as "would be refused"? |
|---|---|---|
| **Refused (403)** | The request got 401, 403 or 429 in audit mode too: sign-in, offboarding, a budget, or an unpriced model. A real refusal | No. Shown on its own line, "Refused for real" |
| **Would be refused** | The request was served, and the rules in force then would have refused it | Yes |
| **Would use another model (unless a later route matched)** | A ModelRouter line was answered, the person may not use that line's model, and enforcing would send them to the default model | No |
| **Mark missing** | A model request or a tool call was served but its record has no valid verdict. The rule expression failed, or the audit fields were not active yet | **No.** Counted on its own line: "Call records with a missing or failed mark" |
| Not a call | `tools/list`, `initialize` and other protocol messages | No. Hidden by default |

A mark that is missing is a defect to fix, not a verdict: in enforce mode the same failure would refuse the request. The summary and `ambit check --production` both show the count of the last 24 hours (`ambit check` prints it as a line under `P11`, and warns above zero). If it stays above zero, report the rule that fails.

A record shows only the router line that answered. A later matching line that the person could use is not visible, which is why the badge says "unless a later route matched".

If a request names a model that several `Model` objects share (the same `spec.model`), audit-only mode serves the first matching route. That can be a different provider than enforce mode would choose for that person. Ambit does not try to repeat enforce mode's choice.

If no record arrives for an hour while audit-only mode is on, the summary shows a warning. If people used Ambit in that time, run `ambit check --production --deep`.

## 3. Write the rules

Use the table to decide which groups need which models and tools, and write the `AccessRule` objects as in [Different models for different teams](models-per-team.md) and [Give an agent tools and apps](mcp-apps.md). Each change of rules gets a new version, and the controller waits for agentgateway to apply it before it publishes the version. If agentgateway does not apply a rules change within 120 seconds, or the API server refuses an object, every AccessRule shows `Ready=False` with the reason `RulesNotActive` and the cause, and the old version stays published.

If the rules produce more "would be refused" fields than the access-log policy has room for (64 attributes in total), every AccessRule shows `Ready=False` with the reason `AuditTooLarge`, and Ambit keeps enforcing. Reduce the number of models, or switch to enforcing. The same happens when the rules for one model or one router are longer than one field holds (16000 characters): the message names that model or router and its size. Grant it to fewer groups and people, give a router fewer targets, or switch to enforcing.

## 4. Switch to enforcing

When the summary with **Only since the current rules** shows 0, switch:

```sh
helm upgrade ambit oci://ghcr.io/craftsman-apps/charts/ambit --version <version> -n ambit-system -f values.yaml --set access.mode=enforce --wait
```

The controller does it in the opposite order: it sets the authorizations first, and then removes the audit fields. Wait until the banner has gone, and `ambit check --production` shows `P11` as passed. The Audit page keeps the summary for 30 days.

## If a switch does not complete

- Read the reason on any AccessRule: `kubectl describe accessrule -n ambit-system <name>`, or the banner.
- A step that agentgateway does not accept in time is tried again with a wait that grows to 5 minutes.
- A change that a gateway proxy rejects (`AgentGatewayNackError` on the Gateway) does not pass on a retry. Fix the cause, or return to the old rules, and the controller starts again from the state that is written.
- Ambit cannot see a proxy that is disconnected from agentgateway. That proxy serves its old configuration for every kind of change.

## Limits

- The mode is cluster-wide. A mode for one team or one rule is not available.
- Budgets are enforced. A shadow mode for budgets is not available.
- Argument limits and tool approval are enforced, and their refusals are not marked.
- `tools/list` is not marked, so the page does not show which tools enforce mode would have hidden.
