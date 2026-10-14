# Contributing to Ambit

Thank you for helping. This page explains how to build Ambit, test it and open a pull request.
Please read the [Code of Conduct](CODE_OF_CONDUCT.md) first.

## Questions and reports

- Questions and ideas: open a GitHub issue on this repository.
- Bugs: open a GitHub issue with the Ambit version, what you did and what you expected.
- Security problems: do not open an issue. Follow [SECURITY.md](SECURITY.md).

## Requirements

Go (the version in `go.mod`), Node.js and npm, Docker, [kind](https://kind.sigs.k8s.io), kubectl, Helm, and [uv](https://docs.astral.sh/uv/) (the tests use it to run Python scripts).

## Build from source

The console embeds the built front end, so build the front end first:

```sh
make web        # npm ci and npm run build in web/
go build ./...
```

Build the image and install it into a local kind cluster:

```sh
make kind-up image install
```

`make kind-up` creates the kind cluster `ambit` from `kind/cluster.yaml` and adds a CoreDNS rewrite so that
`*.localtest.me` names work inside the cluster. `make image` builds `ambit-console:dev` and loads it into the cluster.
`make install` applies the third-party CRDs and installs the two Helm charts, `ambit-crds` first, then `ambit`.
The console is at http://console.localtest.me:8080. `make kind-down` deletes the cluster.

## Run the tests

| What | Command | Needs a cluster |
|---|---|---|
| Unit and integration tests (Go, with an envtest API server) | `make test-go` | no |
| Chart lint | `make lint` | no |
| Acceptance tests, all steps | `make test` | yes, the kind cluster above |
| One acceptance step | `make test STEPS=3b` | yes |
| Quick start on its own kind cluster | `test/quickstart.sh` | creates and deletes its own |
| Browser tests (Playwright) | `make test-ui` | yes |

`make test-go` and `go test ./...` need no cluster and can run in many places at once.

- Preset tests (catalog, contract, live, release gate): [docs/maintainers/preset-testing.md](docs/maintainers/preset-testing.md)

The acceptance tests (`test/step*.sh`) share one kind cluster and replace the installed console. Run them one
at a time. If several people or agents use one machine, run each cluster step under a lock:

```sh
lockf /tmp/ambit-kind.lock sh -c 'make image install && test/step5.sh'
```

Some tests call OpenRouter free models. They use the key in `~/.config/ambit/openrouter-api-key` and skip those
calls when the file does not exist.

## Pull requests

- Work on a branch. Never commit to `main`; changes reach `main` only through pull requests.
- Open the pull request as a draft: `gh pr create --draft`. A draft runs the build, cli-unit, cli-helm-interop,
  cli-local and cli-partial jobs, so you can push fixes freely. Mark it ready for review when the change is complete;
  that also runs the quickstart and end-to-end jobs once (about 50 minutes of CI time).
- One concern per commit. Write each commit message in plain English: what changed and why.
- Add tests with the change, not later. A failing test is revised, never skipped.
- Add a line to `CHANGELOG.md` under "Unreleased" for each change that users can see.
- A change is accepted when `go vet ./...` and `make test-go` pass, the front end builds with `make web` (it type-checks),
  generated files are current, and the change comes with its tests. CI runs these checks and more.
- Before a merge, the branch must contain the current `main` and CI must pass on that head.
- A change that only touches `docs/`, `*.md`, `LICENSE` or `NOTICE` skips the build and end-to-end jobs.
- After you change `api/v1alpha1`, run `make generate` and commit the generated CRDs. CI checks that they are current.

## Sign-off and licence

Ambit does not use the Developer Certificate of Origin: commits need no `Signed-off-by` line.
By contributing, you agree that your contribution is licensed under the [Apache License 2.0](LICENSE).
