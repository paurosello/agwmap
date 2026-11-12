# Working on agwmap

agwmap is a kubectl plugin in one Go package: `collect.go` reads every kind under the gateway API
groups, `graph.go` turns the objects into the request flow, `anonymize.go` and `minimize.go` clean a
capture, and `web/index.html` is the page. The README describes the behaviour; keep it in step with
any change a user would notice. `CHANGELOG.md` is written by release-please from the commit
messages: never edit it by hand, write the commit message instead.

## Commit messages

Every commit follows [Conventional Commits](https://www.conventionalcommits.org/): a subject line
`type(scope)!: summary`, a blank line, a body. The scope is optional. The `commits` workflow checks
every commit pushed to `main` or in a pull request, and the pull request title, which becomes the
commit of a squash merge.

| Type | Use for | In the changelog | Next version (before 1.0) |
| --- | --- | --- | --- |
| `feat` | Something a user can now do or see | Features | minor |
| `fix` | A user-visible bug fixed, dependency updates | Bug Fixes | patch |
| `perf` | Faster or smaller, same behaviour | Performance Improvements | patch |
| `refactor`, `test`, `docs`, `build`, `ci`, `chore`, `style` | Everything else | not listed | none |

A `!` after the type, or a `BREAKING CHANGE:` footer, marks a breaking change; before 1.0 it bumps
the minor version. The `feat` and `fix` subjects are the release notes, so write them for someone
who runs agwmap, not for someone who reads the code: "fix: keep the route order of the cluster", not
"fix: sort slice in graph.go". One change per commit, so that each line of the changelog is one
change.

## Checks

```sh
gofmt -l .         # must print nothing
go vet ./...
go test ./...      # builds the binary and runs it against a fake API server that serves testdata/
scripts/e2e.sh     # the same against a kind cluster (needs kind, kubectl, helm, jq, yq); deletes the cluster after
```

Run the first three before every commit. Run `scripts/e2e.sh` when the change touches what is read
from the cluster.

## Rules

- No CRD schema content from Solo Enterprise for agentgateway in the repo, in issues or in pull
  requests: names, kinds, versions and fingerprints only. Test fixtures and examples are written by
  hand, not copied from a chart or a cluster.
- Captures can hold real cluster data. Never commit a page or snapshot that agwmap wrote; fixtures in
  `testdata/` use invented names.
- Commit locally on the current branch. Do not push, tag, open or merge pull requests (the release
  pull request included) unless the owner asked for it.

## An issue labelled `crd-drift`

The daily `crd-watch` workflow opens it when an upstream CRD was added, removed or changed since
`crds.lock.json` was last updated. The issue lists the CRDs by name. To handle it:

1. `scripts/crds.sh check` prints the current report; `scripts/crds.sh diff <crd-name>` prints what
   changed in that CRD's schema between the locked and the latest version (`diff` alone covers every
   changed CRD). The output is for reading locally; do not paste it into the issue or a commit.
2. Decide whether the map is affected. A new kind needs a drawing rule in `graph.go` only if it takes
   part in the request flow or attaches to something that does; otherwise it already appears in the
   Resources and Raw tabs. A changed kind matters when a field that `graph.go` or `web/index.html`
   reads was renamed, removed or moved, or when a new field belongs on a card.
3. If it is affected: change the code, add the case to `testdata/` and `e2e_test.go`, and update the
   "Supported resources" tables in the README, including the version they say they were checked
   against. Commit it as `feat:` (a new kind or field on the map) or `fix:` (a field that moved) so
   that it reaches the changelog.
4. `scripts/crds.sh update`, then commit `crds.lock.json` together with the fix, or on its own as
   `chore:` with a body saying why nothing had to change. That commit is what stops the issue from
   being raised again; closing the issue without it does not.

## Dependency updates

Renovate opens them weekly (`renovate.json`) with Conventional Commit titles (`fix(deps):` for Go
modules, `chore(deps):` for GitHub Actions). A pull request that passes `ci` and `commits` is fine to
merge; the `k8s.io` modules move together in one.

## Releasing

Releases are cut by release-please (`release-please-config.json`, `.release-please-manifest.json`).
On every push to `main` it opens or updates a pull request titled `chore(main): release X.Y.Z`,
which sets the version in the manifest and adds that version's section to `CHANGELOG.md`.
The `feat`, `fix` and `perf` commits since the last release decide the version and the text.

Merging that pull request is the release, so only the owner merges it. release-please then tags
`vX.Y.Z` and publishes the GitHub release with that section as notes, and calls the `release`
workflow. That workflow tests, builds the archives, attaches them to the release, and commits
`plugins/agwmap.yaml` to `main` so that krew installs the new version.

- Never edit `plugins/agwmap.yaml`, `CHANGELOG.md` or `.release-please-manifest.json` by hand, and
  never push a release tag yourself. To change what a release says, change the release pull request
  or the commit messages, not the files.
- The release pull request is opened with the workflow token, so `ci` does not run on it; it only
  changes the changelog and the manifest, and `main` was green when it was last updated.
- A failed `release` job can be re-run from the Actions tab, or by pushing the same tag again; every
  step repeats safely.

`scripts/release.sh v0.0.0` builds the same archives locally into `dist/` to check that they build;
discard the `plugins/agwmap.yaml` it writes.
