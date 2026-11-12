# Working on agwmap

agwmap is a kubectl plugin in one Go package: `collect.go` reads every kind under the gateway API
groups, `graph.go` turns the objects into the request flow, `anonymize.go` and `minimize.go` clean a
capture, and `web/index.html` is the page. The README describes the behaviour; keep it and
`CHANGELOG.md` (under `## [Unreleased]`) in step with any change a user would notice.

## Checks

```sh
gofmt -l .         # must print nothing
go vet ./...
go test ./...      # builds the binary and runs it against a fake API server that serves testdata/
scripts/e2e.sh     # the same against a kind cluster (needs kind, kubectl, helm, jq); deletes the cluster after
```

Run the first three before every commit. Run `scripts/e2e.sh` when the change touches what is read
from the cluster.

## Rules

- No CRD schema content from Solo Enterprise for agentgateway in the repo, in issues or in pull
  requests: names, kinds, versions and fingerprints only. Test fixtures and examples are written by
  hand, not copied from a chart or a cluster.
- Captures can hold real cluster data. Never commit a page or snapshot that agwmap wrote; fixtures in
  `testdata/` and `examples/` use invented names.
- Commit locally on the current branch. Do not push, tag, open pull requests or publish a release
  unless the owner asked for it.

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
   against, and the changelog.
4. `scripts/crds.sh update`, then commit `crds.lock.json` together with the fix, or on its own with a
   message saying why nothing had to change. That commit is what stops the issue from being raised
   again; closing the issue without it does not.

## Dependency updates

Renovate opens them weekly (`renovate.json`). A pull request that passes `ci` is fine to merge; the
`k8s.io` modules move together in one.

## Releasing

Only when the owner asks, and from an up-to-date `main` with `ci` green.

1. In `CHANGELOG.md`, rename `## [Unreleased]` to `## [X.Y.Z] - YYYY-MM-DD` (the date from `date
   +%F`), add an empty `## [Unreleased]` above it, and update the link definitions at the bottom of
   the file. The release notes are the text of that section.
2. Commit, and push `main`.
3. `git tag vX.Y.Z && git push origin vX.Y.Z`. The `release` workflow tests, builds the archives,
   publishes the GitHub release and commits `plugins/agwmap.yaml` to `main` so that krew installs the
   new version. Never edit that file by hand. The workflow can be run again for the same tag.

`scripts/release.sh v0.0.0` builds the same archives locally into `dist/` to check that they build;
discard the `plugins/agwmap.yaml` it writes.
