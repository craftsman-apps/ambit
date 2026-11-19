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

## Release order

The website, the Helm charts, the image and the CLI are all released from one tagged commit. Merging to `main`
deploys nothing.

1. Merge every pull request for the release into `main`, including its docs.
   Run `node site/scripts/changelog-assemble.mjs 0.2.0 2026-10-20` (your version and date). It moves the files in `changelog/unreleased/` into a new section of `CHANGELOG.md` and deletes them.
   In the same PR, update the version in `docs/install-by-hand.md` and `docs/install.md`; the site build fails otherwise.
   The site Releases section (one page per version) is generated from `CHANGELOG.md` (without "Unreleased") and appears at the next `v*` or `site-*` tag.
2. Push the tag on that commit: `git tag v0.2.0 <commit>` then `git push origin v0.2.0`. The `release` workflow
   publishes the image, the charts and the CLI; the `site` workflow deploys ambitproject.dev from the same commit.
   A prerelease tag (`v0.2.0-rc.1`) publishes to GitHub only and does not deploy the site.
3. Approve the `cli-channels` job (Homebrew, npm, Scoop and winget) after the checksum check passes. Before it
   writes any channel, the job proves with `gh attestation verify` that `checksums.txt` was made by this repository's
   release workflow. Only the highest stable tag reaches the channels: a re-run of an older tag's workflow is not
   marked latest and skips `cli-channels` (#108 W2).
   Then the `cli-windows` job checks the Windows channels on a Windows runner: the release zip, `install.ps1`,
   `scoop install ambit/ambit` and `winget validate` of the manifests.
   Record here: "winget PR merged (link)". The release does not wait for Microsoft's review.

4. After the release: `cosign verify` from `docs/compliance.md` (Supply chain) passes for the new image tag.

## npm trusted publishing (#148)

The npm step publishes through trusted publishing (GitHub OIDC, no stored token) and falls back to the secret
`NPM_TOKEN` when npm refuses it. Do these steps once; the secret goes only after a release has published without it.

1. On npmjs.com, for each of the seven packages (`@craftsman-apps/ambit`, `@craftsman-apps/ambit-darwin-arm64`,
   `@craftsman-apps/ambit-darwin-x64`, `@craftsman-apps/ambit-linux-arm64`, `@craftsman-apps/ambit-linux-x64`,
   `@craftsman-apps/ambit-win32-arm64`, `@craftsman-apps/ambit-win32-x64`): open the package, Settings, Trusted
   Publisher, GitHub Actions. Enter organization `craftsman-apps`, repository `ambit`, workflow filename `release.yml`,
   environment `release-channels`. Save.
2. Release as usual. Then check one package: `npm view @craftsman-apps/ambit@<version> _npmUser`. A trusted publish
   names GitHub Actions as the publisher; a publish with the token names the npm user who owns the token.
3. When all seven show the trusted publisher: delete the secret `NPM_TOKEN` (GitHub, Settings, Environments,
   `release-channels`), revoke the token on npmjs.com (Access Tokens), and remove the `NODE_AUTH_TOKEN` line from the
   npm step of `.github/workflows/release.yml`. Optionally set each package to "Require two-factor authentication and
   disallow tokens" (package Settings, Publishing access).

A new package (a new platform) cannot use trusted publishing for its first publish; it needs a token once.

If the `cli` job fails, the release stays not-latest (the `cli` job uploads the CLI archives, attests them, and only then marks
the release as the latest), and `install.sh` and `install.ps1` keep installing the previous release. Re-run the job.
If you fixed the cause by hand instead, run `gh release edit <tag> --latest`.

To publish a homepage or docs fix between releases, push a site tag: `git tag site-2026-10-20 <commit>` then
`git push origin site-2026-10-20`. The site tag publishes every page on that commit, so pick a commit whose docs do
not describe unreleased features (for example a branch made from the last `v*` tag with only the fix on it).

## Before the first release with the CLI (v0.2.0)

Do these once, before the tag `v0.2.0` is pushed. `v0.1.0` had no CLI, so none of this has been done for it.

1. Check P1 on a real install with 2 replicas (`gateway.replicas: 2`). The PodDisruptionBudget label comes from agentgateway's controller, so only a real install shows that P1 finds the budget: `ambit check --production` must print `✓ P1`.
2. Open USER ACTIONs (a maintainer does them in GitHub and npm; the release workflow cannot run without them):
   - Create the repository `homebrew-tap` and the secret `HOMEBREW_TAP_TOKEN` that can push to it.
   - Create the Environment `release-channels` with the maintainer as required reviewer.
   - Create the npm organization `craftsman-apps` and a granular token with read and write on `@craftsman-apps`; store it as the secret `NPM_TOKEN` in `release-channels` (the npm step passes it as `NODE_AUTH_TOKEN`). Trusted publishing can replace it after the first release.
   - Make `cli-unit`, `cli-local` and `cli-partial` required checks of `main`.
3. At the first tag, confirm in the `cli` job log that `attest-build-provenance` attested every archive (its `subject-path` is a comma-separated list, which runs for the first time there): `gh attestation verify <archive> --repo craftsman-apps/ambit` passes for one archive of each system.

## Scoop and winget (spec D2 8.4)

The owner has no Windows machine, so the Windows checks run on GitHub's `windows-latest` runners. Owner actions,
once:

| # | Action | Exact steps |
|---|---|---|
| O1 | Scoop bucket repository | GitHub, new repository `craftsman-apps/scoop-ambit`, public, with a README and an empty `bucket/` folder. |
| O2 | Scoop token | GitHub, Settings, Developer settings, Fine-grained tokens, new token: resource owner `craftsman-apps`, only the repository `scoop-ambit`, permission Contents read and write, expiry 1 year. Save it as the secret `SCOOP_BUCKET_TOKEN` in Settings, Environments, `release-channels`. |
| O4 | winget token | Create a bot GitHub account that the owner controls (two-factor sign-in) and that owns nothing else; fork `microsoft/winget-pkgs` with it. Create a classic token on it with only `public_repo`, expiry 1 year (fine-grained tokens cannot open pull requests in another owner's repository). Save it as `WINGET_TOKEN` in `release-channels`. Never the owner's own account: `public_repo` can write to every public repository the account can write to. |
| O3 | First winget submission | After a stable release is published and O4 is done: Actions, `winget-first`, Run workflow, version `0.5.0`. It runs on `windows-latest`, checks the attestation of `checksums.txt`, writes the manifests (`scripts/winget/manifest.mjs`), runs `winget validate`, and opens the pull request with `wingetcreate submit`. Microsoft reviews it; expect days. |
| O5 | Windows check | Each release: the `cli-windows` job of the release workflow (it replaces the hand test). Read its log. |

Until these actions are done, the release skips the channel with a notice and does not fail (the test
`.github/scripts/release-skip_test.sh` runs these paths in CI). The notices in the release log:

| Missing | Job, step | Notice |
|---|---|---|
| O1 or O2 | `cli-channels`, Scoop bucket | `SCOOP_BUCKET_TOKEN is not set: Scoop skipped (owner actions O1, O2)` |
| O1 or O2 | `cli-windows`, scoop install | `craftsman-apps/scoop-ambit has no bucket/ambit.json of version <version> (found: none): scoop install skipped (owner actions O1, O2)` |
| O4 | `cli-channels`, winget pull request | `WINGET_TOKEN is not set: winget skipped (owner action O4)` |
| O3 | `cli-channels`, winget pull request | `CraftsmanApps.Ambit is not in microsoft/winget-pkgs yet: run the winget-first workflow (owner action O3)` |

After Microsoft merges the first pull request (O3), later releases open their winget pull request with Komac from
the `cli-channels` job, without a code change.
