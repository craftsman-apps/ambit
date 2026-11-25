# Merging a pull request

This page is for people who merge pull requests into `main`. It describes merging with GitHub's **merge queue**.
Until the owner turns the queue on (see [Settings for the queue](#settings-for-the-queue)), follow the merge rules in
`CLAUDE.md`.

## What the queue does

Think of a queue at a counter. You do not push your change to the front by yourself; you join the line, and GitHub
serves one change (or a small group) at a time. For each place in the line, GitHub makes a temporary branch: the
current `main` plus your pull request (plus the pull requests ahead of you). It runs the full CI on that branch. If the
required checks pass, GitHub merges. If they fail, GitHub removes your pull request from the line and `main` stays as it
was.

The result: `main` is only ever changed by a commit that has been tested together with the current `main`. You do not
need to update your branch from `main` any more, and you do not need to wait for other people to finish merging first.

## Steps

1. Open the pull request as a draft: `gh pr create --draft`. Draft pull requests run the cheap checks. Push fixes freely.
2. When the change is complete, write the changelog fragment if users can see the change (`changelog/unreleased/`),
   then mark it ready: `gh pr ready <number>`. This runs quickstart and e2e on the pull request once.
3. Run the Codex review and fix what it finds.
4. When every check of the pull request is green, join the queue. Name the commit you reviewed, so a late push cannot slip in:

   ```
   gh pr merge <number> --merge --auto --match-head-commit <the head commit you reviewed>
   ```

   `--merge` is the only merge method (a merge commit). `--auto` means "merge when the queue says the checks passed".
5. Watch the pull request. GitHub builds the queue branch (`gh-readonly-queue/main/pr-<number>-...`) and runs the `ci`
   workflow on it. It looks the same as a pull request run, with the event `merge_group`. When the checks pass, GitHub
   merges and closes the pull request. Then remove your worktree.
6. If the queue run fails, GitHub takes your pull request out of the queue and posts why. Open the failed run, fix the
   cause on your branch, push, wait for the pull request checks, and join the queue again. Do not re-run a failed job
   without reading why it failed; a flaky test gets an issue.

Never use `--admin`, never push to `main`, and never create tags: those rules do not change.

## What replaces the old files

| Before | Now |
|---|---|
| `/tmp/ambit-merge-queue` (a file of pull request numbers; only the first line may merge) | The GitHub merge queue. The order is the order in which pull requests join. Do not write to the file any more. |
| `/tmp/ambit-merge.lock` (a lock held for the last step) | Not needed. GitHub merges one queue entry at a time. |
| `gh pr update-branch` and "wait for CI again when `main` moved" | Not needed. The queue tests your change on top of the current `main`. |
| "CI green on a head that contains the current `main`" | The queue's own run is that proof. |

The kind cluster lock (`/tmp/ambit-kind.lock`) is for local clusters and does not change.

## Which checks must pass

One name is the proof that the whole CI passed: **`merge-gate`** (the last job of `ci.yml`). It waits for every other job
and fails when a job failed, was cancelled, or was skipped although `.github/scripts/changes.sh` says the change needs
it. A job skipped on purpose (for example e2e for a change to documentation only) counts as passed. This is the old
rule, "skipped never counts unless the path filter skips it on purpose", written as code and tested in
`.github/scripts/merge-gate_test.sh`.

Why not require `e2e (a)` to `e2e (e)` by name: when e2e is skipped on purpose, GitHub reports one check named
`e2e (${{ matrix.part.name }})`, and the five part names never arrive. A required check that never arrives blocks the
queue for ever. `merge-gate` always arrives.

How the same jobs run for a queue branch: `ci.yml` also listens for `merge_group`. The job `changes` compares the
queue branch with the branch it merges into (`.github/scripts/changed-files.sh`), so the path filters work the same as
for a pull request, and a filter never skips a job by mistake: when the comparison fails, every job runs.
`.github/scripts/changed-files_test.sh` checks this on real commits: a pull request and a merge group for the same
change run the same jobs, also when `main` moved in between.

## Settings for the queue

Only the owner changes the repository rules. The recommended values:

| Setting | Value | Why |
|---|---|---|
| Required checks | `merge-gate`, `build`, `cli-unit (ubuntu-latest)`, `cli-unit (macos-latest)`, `cli-unit (windows-latest)`, `cli-local`, `cli-partial` | `merge-gate` covers quickstart and e2e. The other six are required today. Do not add the names `e2e (a)` to `e2e (e)`. |
| Merge method | Merge commit | The merge rule for this repository. |
| Maximum pull requests to build | 3 | Up to three queue branches are tested at the same time; each one includes the ones ahead of it. |
| Maximum group size | 3 | Up to three pull requests are merged by one run. One e2e run takes about 50 minutes of Actions time, so grouping saves minutes. |
| Minimum group size | 1 | A single pull request does not wait for others. |
| Wait time to meet the minimum group size | 5 minutes | A short wait lets pull requests that are ready together share a run. |
| Status check timeout | 120 minutes | The e2e parts take 15 to 20 minutes when a runner is free. The runner pool is shared, and jobs have waited more than an hour for a runner. |
| Pull requests that fail in a group | Require all entries to pass (the default) | A group that fails is split, and its pull requests are tested again without the failing one. |

One cost to know: the required checks apply to the pull request too, so e2e runs once on the pull request (when it is
marked ready) and once more in the queue. That is the price of testing together with the current `main`. To avoid the
first run, the pull request could run only the cheap jobs; this needs a decision about what a pull request must prove
before it joins the queue.
