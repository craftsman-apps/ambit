# Release checklist: the ambit CLI by hand

GitHub's macOS and Windows runners cannot run the Linux containers kind needs, so `ambit local up` is tested in CI
on Linux only. Before each release, a person runs it on macOS and Windows from the release candidate and records the
result here (spec of the CLI, section 9.3).

1. Build the candidate: `goreleaser release --snapshot --clean`, or download the archives of the release candidate.
2. macOS (Docker Desktop or OrbStack), with no cluster named `ambit-rc`:
   - `ambit local up --name ambit-rc --port 18080 --https-port 18443 --no-key --chart-dir charts` (a snapshot) or without `--chart-dir` (a tagged candidate): eight `✓` lines and the summary.
   - Open the console address from the summary, sign in, find the test call in Audit.
   - `ambit local down --name ambit-rc --yes`: `✓ Deleted kind cluster ambit-rc`.
   - For a tagged candidate, also `brew install craftsman-apps/tap/ambit` and `ambit version` (the formula installs and the binary runs without a warning), then `brew uninstall ambit`.
3. Windows (Docker Desktop with WSL 2), in PowerShell: the same three steps; the summary's call command is PowerShell.
4. Record: date, version, system, Docker version, result.

| Date | Version | System | Docker | Result |
|---|---|---|---|---|
