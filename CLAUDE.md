# Working on Ambit with Claude Code

## Worktrees

- Every change gets its own git worktree inside this repository, at `.worktrees/<branch>`:

  ```
  git fetch origin
  git worktree add .worktrees/<branch> -b <branch> origin/main
  ```

  `.worktrees/` is in `.gitignore`; never commit it.
- One session, one worktree, one branch. Never edit another session's worktree or branch.
- Never commit to `main`. Changes reach `main` through pull requests only.
- Remove a worktree after its pull request is merged: `git worktree remove .worktrees/<branch>`.

## The local kind cluster is shared

All sessions use the same kind cluster (`make kind-up`: cluster `ambit`, namespace `ambit-system`, image
`ambit-console:dev`, host ports 8080 and 8443). One session's `make image install` replaces another session's
console in the middle of its tests. Run cluster steps (image build, install, `make test`, `test/step*.sh`,
`make test-ui`) one session at a time, as one locked unit:

```
lockf /tmp/ambit-kind.lock sh -c 'make image install && test/step5.sh'
```

A second session's command waits until the first one finishes. Unit tests and envtest
(`go test ./...`) need no cluster and can run in any number of sessions at once.
