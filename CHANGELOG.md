# Changelog

All notable changes to agwmap are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

First release.

### Added

- Capture of every object under the Gateway API, agentgateway (open source and enterprise), kgateway
  and inference extension API groups. Read-only; Secrets, ConfigMaps and Services are never read.
- Request flow page: per gateway, listener and route, the listener, the policies attached to the
  gateway, the route, the policies attached to the route, and per rule the filters, backends, their
  policies and upstream targets.
- Listener sets, route delegation, routes attached to a Service (mesh), inference pools, and backends
  that are another gateway. A route is shown under the listeners that take it (section name, port,
  protocol and `allowedRoutes`).
- Objects a card refers to (auth configs, rate limit configs, WAF policies, budgets, models, external
  secrets, parameters) nested under it; uncaptured Secrets and Services shown as names.
- Flags for what needs attention: failing status conditions, cross-namespace references without a
  ReferenceGrant, the same setting configured at more than one level, policies whose target is missing.
- "Outside the flow" tab with every captured object that no request reaches, and why.
- "Resources" tab with all captured objects and "Raw" tab with their YAML.
- Anonymization, on by default: hostnames, URLs, IP addresses, account and client ids and inline
  credentials become stable pseudonyms; metadata is reduced to name, namespace and gateway labels and
  annotations. Object names and namespaces are kept. `-anonymize=false` keeps the real values.
- Snapshots: export the capture from the page or with `-o file.json`, render it again with `-from`.
- kubectl plugin: installed as `kubectl-agwmap` it runs as `kubectl agwmap`, with `-n`/`--namespace`,
  `-A`/`--all-namespaces`, `--context` and `--kubeconfig`. A krew manifest is published in `plugins/`
  with each release.
- End-to-end tests, run on every push and pull request: the binary against a fake API server, and
  against a kind cluster with `examples/demo.yaml`.

[Unreleased]: https://github.com/paurosello/agwmap/commits/main
