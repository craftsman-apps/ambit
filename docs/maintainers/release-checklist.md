# Release checklist: the ambit CLI by hand

GitHub's macOS and Windows runners cannot run the Linux containers kind needs, so `ambit local up` is tested in CI
on Linux only. Before each release, a person runs it on macOS and Windows from the release candidate and records the
result here (spec of the CLI, section 9.3).

1. Build the candidate: `goreleaser release --snapshot --clean`, or download the archives of the release candidate.
2. macOS (Docker Desktop or OrbStack), with no cluster named `ambit-rc`:
   - `ambit local up --name ambit-rc --port 18080 --https-port 18443 --no-key --chart-dir charts` (a snapshot) or without `--chart-dir` (a tagged candidate): seven `✓` lines and the summary.
   - Open the console address from the summary, sign in, find the test call in Audit.
   - `ambit local down --name ambit-rc --yes`: `✓ Deleted kind cluster ambit-rc`.
   - For a tagged candidate, also `brew install craftsman-apps/tap/ambit` and `ambit version` (the formula installs and the binary runs without a warning), then `brew uninstall ambit`.
3. Windows (Docker Desktop with WSL 2), in PowerShell: the same three steps; the summary's call command is PowerShell.
4. Record: date, version, system, Docker version, result.

| Date | Version | System | Docker | Result |
|---|---|---|---|---|

## Before the first `v*` tag

Do these once, before the first release tag is pushed.

1. Check P1 on a real install with 2 replicas (`gateway.replicas: 2`). The PodDisruptionBudget label comes from agentgateway's controller, so only a real install shows that P1 finds the budget: `ambit check --production` must print `✓ P1`.
2. Open USER ACTIONs (a maintainer does them in GitHub and npm; the release workflow cannot run without them):
   - Create the repository `homebrew-tap` and the secret `HOMEBREW_TAP_TOKEN` that can push to it.
   - Create the Environment `release-channels` with the maintainer as required reviewer.
   - Create the npm scope `@craftsman-apps`. Use trusted publishing, or create an `NPM_TOKEN` and wire it as `NODE_AUTH_TOKEN` on the npm step of the release workflow.
   - Make `cli-unit`, `cli-local` and `cli-partial` required checks of `main`.
