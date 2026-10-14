# Release checklist (maintainers)

### The cli job fails

The `cli` job in `.github/workflows/release.yml` uploads the CLI archives, attests them, and only then marks the
release as the latest. If the job fails, the release stays not-latest, and `install.sh` and `install.ps1` keep
installing the previous release. Re-run the job. If you fixed the cause by hand instead, run
`gh release edit <tag> --latest`.
