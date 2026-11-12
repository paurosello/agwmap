# Changelog

All notable changes to agwmap are listed here. Versions follow
[Semantic Versioning](https://semver.org/). From 0.1.0 on, each section is written by release-please
from the [Conventional Commits](https://www.conventionalcommits.org/) merged into the release.

## [0.3.0](https://github.com/paurosello/agwmap/compare/v0.2.0...v0.3.0) (2026-11-12)


### Features

* show a summary of each request flow ([#7](https://github.com/paurosello/agwmap/issues/7)) ([633a9c0](https://github.com/paurosello/agwmap/commit/633a9c0bddb8eb5556098986db3f6179958e0017))

## [0.2.0](https://github.com/paurosello/agwmap/compare/v0.1.0...v0.2.0) (2026-11-12)


### ⚠ BREAKING CHANGES

* open the map in the browser and write it to the temp folder by default

### Features

* open the map in the browser and write it to the temp folder by default ([98617ae](https://github.com/paurosello/agwmap/commit/98617ae00aa37f9befc1482866ec448fffd351c8))

## [0.1.0] - 2026-11-12

First release.

### Added

- Capture of every object under the Gateway API, agentgateway (open source and enterprise), kgateway
  and inference extension API groups. Read-only; Secrets, ConfigMaps and Services are never read.
- Request flow page: per gateway, listener and route, the listener, the policies attached to the
  gateway, the route, the policies attached to the route, and per rule the filters, backends, their
  policies and upstream targets.
- Listener sets, route delegation, routes attached to a Service (mesh), inference pools, and backends
  that are another gateway. A route is shown under the listeners that take it (section name, port,
  protocol, `allowedRoutes` and hostname).
- Every Gateway API kind of v1.7.0, standard and experimental: TLS, TCP and UDP routes, ListenerSet
  (flagged when the gateway's `allowedListeners` does not admit it), XBackend, XBackendTrafficPolicy,
  XTelemetryPolicy and XMesh. Per rule: timeouts, retry, the filters of each backendRef and the objects
  a filter refers to. Per listener: its own status conditions.
- Objects a card refers to (auth configs, rate limit configs, WAF policies, budgets, models, external
  secrets, parameters) nested under it; uncaptured Secrets and Services shown as names.
- Flags for what needs attention: failing status conditions, cross-namespace references without a
  ReferenceGrant, the same setting configured at more than one level, policies whose target is missing.
- "Outside the flow" tab with every captured object that no request reaches, and why.
- "Resources" tab with all captured objects and "Raw" tab with their YAML.
- Anonymization, on by default: hostnames, URLs, IP addresses, account and client ids and inline
  credentials become stable pseudonyms; metadata is reduced to name, namespace and gateway labels and
  annotations. Object names and namespaces are kept. `-anonymize=false` keeps the real values.
- Snapshots: export the capture from the page or with `-o file.yaml`, render it again with `-from`. A
  snapshot is a multi-document YAML manifest with one extra document that describes the capture.
- `-from` also renders any other YAML manifest of gateway objects, without a cluster: files written
  for `kubectl apply -f`, `helm template` output, or the list from `kubectl get -o yaml`. Objects
  outside the gateway API groups are skipped.
- kubectl plugin: installed as `kubectl-agwmap` it runs as `kubectl agwmap`, with `-n`/`--namespace`,
  `-A`/`--all-namespaces`, `--context` and `--kubeconfig`. A krew manifest is published in `plugins/`
  with each release.
- A line on stderr when a newer release exists, with the krew command to update. Nothing is printed
  when the check cannot reach GitHub or the output is not a terminal.
- End-to-end tests, run on every push and pull request: the binary against a fake API server, and
  against a kind cluster with `testdata/demo.yaml`.

[0.1.0]: https://github.com/paurosello/agwmap/releases/tag/v0.1.0
