---
title: Do a quarterly access review
description: Export who may use which models and MCP tools on a date, add your identity provider's member list, and give both to your auditor with the file's SHA-256.
---

# Do a quarterly access review

**What you get:** the evidence for an access review (SOC 2 CC6.2 and CC6.3, ISO/IEC 27001 control 5.18) in a few minutes: which groups and people the access rules allowed to use which models, MCP servers and tools at the end of a date, which personal and service account keys were alive, and who actually used each group. You add the member list of each group from your identity provider, and you send the auditor the files and their SHA-256.

An access review needs two inputs, and Ambit has only one of them:

1. **What each group may use.** Ambit knows this: the AccessRules in force on the date.
2. **Who is in each group.** Your identity provider decides this. Ambit keeps no group directory: it reads the groups from each sign-in. Ambit's export shows the people who were *seen* using a group (observed use), which is not the same as the people who *may* use it: a member who never made a call is not in it.

So the first step happens in your identity provider.

## Before you start

You need:

- Ambit 0.5.0 or later with `evidence.enabled: true`. The controller records the access rules in force (an *access snapshot*) at every change and once a day; access reviews are available from the upgrade to 0.5.0 on. There are no snapshots for earlier dates.
- the admin role in the console (group `ambit-admins`). Only admins can export.

## 1. Export each group's members from your identity provider

For each group that appears in your AccessRules, export the member list from your identity provider (Okta and Entra ID both have a member export or a group membership report), for the same date as the review (for a quarter, its last day), with the date on it. Keep these files with the review.

## 2. Export the access review from Ambit

In the console, open **Settings → Evidence**. In **Access review**, the date is the last day of the last full quarter. Change it if needed and select **Download**.

If the page says "No snapshot for this date", the date is before the upgrade to 0.5.0, or before evidence was turned on. Choose a later date.

The ZIP file `ambit-access-<date>.zip` holds:

| File | What it holds |
|---|---|
| `grants.csv` | One row per rule, who (`group`, `user`, `service account` or `all-signed-in`) and what (`model`, `server` for every tool of a server, or `tool`), with argument limits, the rules version and the access mode in force at the end of the date. |
| `keys.csv` | The personal and service account keys alive at the end of the date: key ID, owner, owner type, groups, created, until and `revoked_at` (when it was revoked later). Key values and hashes are never exported. |
| `people-seen.csv` | Per group, the people whose calls carried that group in the 90 days before the end of the date: first and last call, and the number of calls. |
| `manifest.json` | How the file was made: the access snapshot used (its time, rules version and SHA-256), every snapshot of that day, the rows and the SHA-256 of each file, and notes. |

The page shows the ZIP's SHA-256 after the download, and **Export history** keeps it.

## 3. Compare and record

For each row of `grants.csv` with `who_type` `group`, compare the group's member list (step 1) with what the rule grants. Questions to answer, for each group:

- Should every member of this group be able to use these models and tools?
- Are there people in `people-seen.csv` who are not in the member list? (They were in the group earlier in the 90 days, or their sign-in carried the group from another source.)
- Are there keys in `keys.csv` whose owner left, or that nobody uses?

Change the rules, the group membership or the keys as needed, and write down the decisions: the review and its result are your record, not Ambit's.

If `access_mode` is `audit`, the rules were not enforced at that time (see [Turn Ambit on without blocking anyone](audit-only.md)): "may use" was not "was limited to". The manifest notes say so.

## 4. Hand over the files

Send the auditor:

- the ZIP file from Ambit,
- the member lists from your identity provider,
- your notes from step 3,
- the ZIP's SHA-256, in the e-mail or ticket, at the time you send the file. This value, outside the cluster, lets the auditor check later that the file is the one you exported.

What the hashes prove, in plain words: they detect a change or damage of the file after the export. They do not prove that the records in Ambit were never changed: a cluster admin can change records, the export history and a file together. See [Compliance and audits](../compliance.md#evidence-exports).

## Related

- [Compliance and audits](../compliance.md): where all the evidence is, and the format of the three exports.
- [Offboard someone who leaves](someone-leaves.md): the offboarding export.
- [Different models for different teams](models-per-team.md): AccessRules by group.
