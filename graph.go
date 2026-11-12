package main

import (
	"sort"
	"strconv"
	"strings"
	"unicode"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type Meta struct {
	Context     string   `json:"context"`
	Namespace   string   `json:"namespace,omitempty"`
	GeneratedAt string   `json:"generatedAt"`
	Anonymized  bool     `json:"anonymized"`
	Warnings    []string `json:"warnings,omitempty"`
}

type Graph struct {
	Meta  Meta    `json:"meta"`
	Nodes []*Node `json:"nodes"`
	Edges []Edge  `json:"edges"`
}

// Node is one Kubernetes object, or a stub for something that is referred to
// but was not read (a Service, a Secret, an object that does not exist).
type Node struct {
	ID        string         `json:"id"`
	Group     string         `json:"group"`
	Version   string         `json:"version,omitempty"`
	Kind      string         `json:"kind"`
	Namespace string         `json:"ns"`
	Name      string         `json:"name"`
	Role      string         `json:"role"` // class, gateway, route, backend, policy, params, other, external
	External  bool           `json:"external,omitempty"`
	Features  []Feature      `json:"features,omitempty"`
	Obj       map[string]any `json:"obj,omitempty"`
}

// Feature is one capability a policy (or a backend's inline policy) configures.
type Feature struct {
	Category string `json:"cat"`
	Key      string `json:"key"`
	Path     string `json:"path"` // dotted path of the setting inside the object
}

// Edge is a reference from one object to another.
type Edge struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Type    string `json:"type"`              // parent, backend, target, class, params, ref
	Section string `json:"section,omitempty"` // sectionName of the reference
	Path    string `json:"path,omitempty"`    // where in the source object the reference is
	CrossNS bool   `json:"crossNs,omitempty"` // the reference leaves the namespace and needs a ReferenceGrant
	Grant   string `json:"grant,omitempty"`   // the ReferenceGrant that allows it, if any
	Note    string `json:"note,omitempty"`
}

type ref struct {
	group, kind, ns, name string
	inferred              bool // the kind was guessed from the key (wafPolicyRef, modelRef)
}

type builder struct {
	nodes  map[string]*Node
	byKey  map[string][]*Node // kind/ns/name
	byKind map[string][]*Node
	edges  []Edge
	seen   map[Edge]bool
}

func buildGraph(objs []unstructured.Unstructured) *Graph {
	b := &builder{
		nodes:  map[string]*Node{},
		byKey:  map[string][]*Node{},
		byKind: map[string][]*Node{},
		seen:   map[Edge]bool{},
	}
	for i := range objs {
		b.add(nodeFor(&objs[i]))
	}
	// Edges second, so that references resolve against every collected object.
	real := make([]*Node, 0, len(b.nodes))
	for _, n := range b.nodes {
		real = append(real, n)
	}
	sort.Slice(real, func(i, j int) bool { return real[i].ID < real[j].ID })
	for _, n := range real {
		b.link(n)
	}
	b.grants()

	// Empty slices, not nil: the page iterates both, and nil would be JSON null.
	g := &Graph{Nodes: []*Node{}, Edges: append([]Edge{}, b.edges...)}
	for _, n := range b.nodes {
		g.Nodes = append(g.Nodes, n)
	}
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].ID < g.Nodes[j].ID })
	sort.SliceStable(g.Edges, func(i, j int) bool {
		a, c := g.Edges[i], g.Edges[j]
		if a.From != c.From {
			return a.From < c.From
		}
		return a.Path < c.Path
	})
	return g
}

func nodeID(group, kind, ns, name string) string {
	return group + "/" + kind + "/" + ns + "/" + name
}

func nodeFor(u *unstructured.Unstructured) *Node {
	gvk := u.GroupVersionKind()
	obj := u.Object
	n := &Node{
		ID:        nodeID(gvk.Group, gvk.Kind, u.GetNamespace(), u.GetName()),
		Group:     gvk.Group,
		Version:   gvk.Version,
		Kind:      gvk.Kind,
		Namespace: u.GetNamespace(),
		Name:      u.GetName(),
		Obj:       obj,
	}
	spec, _ := obj["spec"].(map[string]any)
	n.Role = roleOf(gvk.Kind, spec)
	switch n.Role {
	case "policy":
		n.Features = features(spec, "spec", "")
	case "backend":
		if p, ok := spec["policies"].(map[string]any); ok {
			n.Features = features(p, "spec.policies", "")
		}
	}
	return n
}

func roleOf(kind string, spec map[string]any) string {
	_, hasRefs := spec["targetRefs"]
	_, hasSel := spec["targetSelectors"]
	switch {
	case kind == "GatewayClass":
		return "class"
	case kind == "Gateway":
		return "gateway"
	case strings.HasSuffix(kind, "ListenerSet"):
		return "listenerset"
	case strings.HasSuffix(kind, "Route"):
		return "route"
	case hasRefs || hasSel:
		// What makes a policy is that it attaches to something. A WAFPolicy or an
		// RBACPolicy is configuration that other objects point at, or none do.
		return "policy"
	case strings.HasSuffix(kind, "Backend") || strings.HasSuffix(kind, "Model") || kind == "InferencePool":
		return "backend"
	case strings.HasSuffix(kind, "Parameters"):
		return "params"
	}
	return "other"
}

func (b *builder) add(n *Node) {
	b.nodes[n.ID] = n
	k := n.Kind + "/" + n.Namespace + "/" + n.Name
	b.byKey[k] = append(b.byKey[k], n)
	b.byKind[n.Kind] = append(b.byKind[n.Kind], n)
}

func (b *builder) edge(e Edge) {
	if e.From == e.To || b.seen[e] {
		return
	}
	b.seen[e] = true
	b.edges = append(b.edges, e)
}

// resolve finds the objects a reference points at. A reference to something
// that was not collected gets a stub node, so dangling references stay visible.
func (b *builder) resolve(r ref, src *Node) []*Node {
	match := func(c *Node) bool { return r.group == "" || c.Group == r.group }
	if r.name == "*" {
		// Wildcard backendRef (AgentgatewayModel): every object of that kind
		// in the namespace that names the source as its parent, or names none.
		var out []*Node
		for _, c := range b.byKind[r.kind] {
			if c.Namespace == r.ns && match(c) && attachesTo(c, src) {
				out = append(out, c)
			}
		}
		return out
	}
	for _, ns := range []string{r.ns, ""} {
		for _, c := range b.byKey[r.kind+"/"+ns+"/"+r.name] {
			if match(c) {
				return []*Node{c}
			}
		}
	}
	if r.inferred {
		// wafPolicyRef names a WAFPolicy and modelRef an AgentgatewayModel: accept a
		// collected kind that equals the guess ignoring case, or ends with it.
		kinds := make([]string, 0, len(b.byKind))
		for k := range b.byKind {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		want := strings.ToLower(r.kind)
		for _, k := range kinds {
			if !strings.HasSuffix(strings.ToLower(k), want) {
				continue
			}
			for _, c := range b.byKey[k+"/"+r.ns+"/"+r.name] {
				if !c.External {
					return []*Node{c}
				}
			}
		}
	}
	stub := &Node{
		ID: nodeID(r.group, r.kind, r.ns, r.name), Group: r.group, Kind: r.kind,
		Namespace: r.ns, Name: r.name, Role: "external", External: true,
	}
	b.add(stub)
	return []*Node{stub}
}

func attachesTo(c, parent *Node) bool {
	spec, _ := c.Obj["spec"].(map[string]any)
	prefs, _ := spec["parentRefs"].([]any)
	if len(prefs) == 0 {
		return true
	}
	for _, p := range prefs {
		m, _ := p.(map[string]any)
		ns, _ := m["namespace"].(string)
		if ns == "" {
			ns = c.Namespace
		}
		if m["name"] == parent.Name && m["kind"] == parent.Kind && ns == parent.Namespace {
			return true
		}
	}
	return false
}

func (b *builder) link(n *Node) {
	spec, _ := n.Obj["spec"].(map[string]any)
	if spec == nil {
		return
	}
	if cls, _ := spec["gatewayClassName"].(string); cls != "" && n.Kind == "Gateway" {
		for _, t := range b.resolve(ref{group: "gateway.networking.k8s.io", kind: "GatewayClass", name: cls}, n) {
			b.edge(Edge{From: n.ID, To: t.ID, Type: "class", Path: "spec.gatewayClassName"})
		}
	}
	if sels, ok := spec["targetSelectors"].([]any); ok {
		for i, s := range sels {
			b.linkSelector(n, s, "spec.targetSelectors."+strconv.Itoa(i))
		}
	}
	b.linkBudgets(n, spec)
	b.walk(n, spec, []string{"spec"})
}

// linkBudgets connects a policy that enforces budgets to the budget objects it
// discovers. Budgets are found by namespace, not by name.
func (b *builder) linkBudgets(n *Node, spec map[string]any) {
	enf, ok, _ := unstructured.NestedMap(spec, "traffic", "entBudgetEnforcement")
	if !ok {
		return
	}
	from, _, _ := unstructured.NestedString(enf, "discovery", "namespaces", "from")
	note := ""
	if from == "Selector" {
		// Namespaces are not captured, so their labels cannot be matched here.
		note = "namespace selector not evaluated: every captured budget is listed"
	}
	for kind, list := range b.byKind {
		if !strings.HasSuffix(kind, "Budget") {
			continue
		}
		for _, c := range list {
			if c.External || ((from == "" || from == "Same") && c.Namespace != n.Namespace) {
				continue
			}
			b.edge(Edge{From: n.ID, To: c.ID, Type: "discovers", Path: "spec.traffic.entBudgetEnforcement", Note: note})
		}
	}
}

// grants marks every reference that leaves its namespace and looks up the
// ReferenceGrant that allows it.
func (b *builder) grants() {
	var extra []Edge
	for i := range b.edges {
		e := &b.edges[i]
		from, to := b.nodes[e.From], b.nodes[e.To]
		if (e.Type != "backend" && e.Type != "ref") || from.Namespace == "" || to.Namespace == "" || from.Namespace == to.Namespace {
			continue
		}
		if to.Role == "route" {
			continue // delegation: the child route agrees by naming its parent, no grant involved
		}
		e.CrossNS = true
		for _, g := range b.byKind["ReferenceGrant"] {
			if g.External || g.Namespace != to.Namespace {
				continue
			}
			spec, _ := g.Obj["spec"].(map[string]any)
			if grantMatches(spec["from"], from.Group, from.Kind, from.Namespace, "") &&
				grantMatches(spec["to"], to.Group, to.Kind, "", to.Name) {
				e.Grant = g.ID
				extra = append(extra, Edge{From: g.ID, To: to.ID, Type: "grant", Path: "spec.to"},
					Edge{From: g.ID, To: from.ID, Type: "grant", Path: "spec.from"})
				break
			}
		}
	}
	for _, e := range extra {
		b.edge(e)
	}
}

func grantMatches(list any, group, kind, ns, name string) bool {
	items, _ := list.([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		g, _ := m["group"].(string)
		k, _ := m["kind"].(string)
		if g != group || k != kind {
			continue
		}
		if ns != "" && m["namespace"] != ns {
			continue
		}
		if n, _ := m["name"].(string); name != "" && n != "" && n != name {
			continue
		}
		return true
	}
	return false
}

func (b *builder) linkSelector(n *Node, s any, path string) {
	m, _ := s.(map[string]any)
	kind, _ := m["kind"].(string)
	group, _ := m["group"].(string)
	section, _ := m["sectionName"].(string)
	want, _ := m["matchLabels"].(map[string]any)
	for _, c := range b.byKind[kind] {
		if c.External || c.Namespace != n.Namespace || (group != "" && c.Group != group) {
			continue
		}
		labels, _, _ := unstructured.NestedStringMap(c.Obj, "metadata", "labels")
		ok := true
		for k, v := range want {
			if labels[k] != v {
				ok = false
				break
			}
		}
		if ok {
			b.edge(Edge{From: n.ID, To: c.ID, Type: "target", Section: section, Path: path})
		}
	}
}

// walk finds object references anywhere in a spec: a map with a name and
// either a kind or a key that ends in Ref/Refs.
func (b *builder) walk(n *Node, v any, path []string) {
	switch t := v.(type) {
	case []any:
		for i, e := range t {
			b.walk(n, e, append(path, strconv.Itoa(i)))
		}
	case map[string]any:
		if r, typ, ok := asRef(n, t, path); ok {
			section, _ := t["sectionName"].(string)
			for _, target := range b.resolve(r, n) {
				b.edge(Edge{From: n.ID, To: target.ID, Type: typ, Section: section, Path: strings.Join(path, ".")})
			}
			return
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if k == "targetSelectors" {
				continue
			}
			// A Secret named by a plain string (elicitation.secretName).
			if s, ok := t[k].(string); ok && s != "" && (k == "secretName" || strings.HasSuffix(k, "SecretName")) {
				for _, target := range b.resolve(ref{kind: "Secret", ns: n.Namespace, name: s}, n) {
					b.edge(Edge{From: n.ID, To: target.ID, Type: "ref", Path: strings.Join(append(path, k), ".")})
				}
				continue
			}
			b.walk(n, t[k], append(path, k))
		}
	}
}

func asRef(n *Node, m map[string]any, path []string) (ref, string, bool) {
	name, _ := m["name"].(string)
	if name == "" {
		return ref{}, "", false
	}
	key := ""
	for i := len(path) - 1; i >= 0; i-- {
		if _, err := strconv.Atoi(path[i]); err != nil {
			key = path[i]
			break
		}
	}
	isRefKey := strings.HasSuffix(key, "Ref") || strings.HasSuffix(key, "Refs")
	kind, _ := m["kind"].(string)
	group, hasGroup := m["group"].(string)
	if kind == "" && !isRefKey {
		return ref{}, "", false
	}
	typ, inferred := "ref", false
	switch key {
	case "parentRefs", "parentRef":
		typ = "parent"
		if kind == "" {
			kind = "Gateway"
		}
		if !hasGroup {
			group = "gateway.networking.k8s.io"
		}
	case "backendRefs", "backendRef":
		typ = "backend"
		if kind == "" {
			kind = "Service"
		}
	case "targetRefs", "targetRef":
		typ = "target"
	case "parametersRef":
		typ = "params"
	case "certificateRefs", "caCertificateRefs":
		if kind == "" {
			kind = "Secret"
		}
	case "endpointPickerRef": // InferencePool: the picker is a Service
		if kind == "" {
			kind = "Service"
		}
	}
	if kind == "" {
		// secretRef -> Secret, authConfigRef -> AuthConfig
		base := strings.TrimSuffix(strings.TrimSuffix(key, "s"), "Ref")
		if base == "" {
			return ref{}, "", false
		}
		r := []rune(base)
		r[0] = unicode.ToUpper(r[0])
		kind = string(r)
		if strings.HasSuffix(kind, "Secret") || strings.HasSuffix(kind, "Certificate") || strings.HasSuffix(kind, "Key") {
			kind = "Secret" // clientSecretRef, mtlsCertificateRef, signingKeyRef
		}
		inferred = kind != "Secret"
	}
	ns, _ := m["namespace"].(string)
	if ns == "" {
		ns = n.Namespace
	}
	return ref{group: group, kind: kind, ns: ns, name: name, inferred: inferred}, typ, true
}

// Settings that only say where a policy applies, not what it does.
var notFeatures = map[string]bool{
	"targetRefs": true, "targetSelectors": true, "strategy": true, "phase": true,
	"description": true, "importScope": true, "imports": true,
}

// Keys that group other settings; their children are the features.
var containers = map[string]bool{"traffic": true, "backend": true, "frontend": true, "ai": true, "mcp": true}

var categories = map[string]string{
	"jwtAuthentication": "authn", "apiKeyAuthentication": "authn", "basicAuthentication": "authn",
	"extAuth": "authn", "entExtAuth": "authn", "authentication": "authn", "jwt": "authn",
	"apiKeyAuth": "authn", "basicAuth": "authn", "oauth2": "authn",

	"authorization": "authz", "networkAuthorization": "authz", "rbac": "authz",

	"rateLimit": "ratelimit", "entRateLimit": "ratelimit", "entBudgetEnforcement": "ratelimit",

	"promptGuard": "guardrails", "entPromptGuard": "guardrails", "entWAF": "guardrails",
	"waf": "guardrails", "guardrail": "guardrails", "guardrails": "guardrails",

	"transformation": "transform", "transformations": "transform", "finalTransformations": "transform",
	"headerModifiers": "transform", "hostRewrite": "transform", "prompt": "transform",
	"defaults": "transform", "overrides": "transform", "modelAliases": "transform",
	"promptCaching": "transform", "headers": "transform", "directResponse": "transform",
	"promptEnrichment": "transform", "routes": "transform",

	"retry": "resilience", "timeouts": "resilience", "health": "resilience", "buffer": "resilience",
	"delay": "resilience", "entHealthCheck": "resilience", "sessionAffinity": "resilience",
	"compression": "resilience", "loadBalancer": "resilience", "healthCheck": "resilience",
	"outlierDetection": "resilience", "circuitBreakers": "resilience",

	"auth": "identity", "tokenExchange": "identity", "entTokenExchange": "identity",
	"entConnector": "identity", "entCrossAppAccess": "identity", "entElicitation": "identity",
	"workloadIdentity": "identity", "entAws": "identity",

	"tls": "security", "cors": "security", "csrf": "security", "proxyProtocol": "security",
	"validation": "security",

	"tracing": "observability", "accessLog": "observability", "metrics": "observability",

	"extProc": "extension", "entPlugin": "extension",

	"http": "connection", "tcp": "connection", "connect": "connection", "tunnel": "connection",
}

func features(m map[string]any, path, prefix string) []Feature {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []Feature
	for _, k := range keys {
		if notFeatures[k] {
			continue
		}
		if child, ok := m[k].(map[string]any); ok && containers[k] {
			p := ""
			if k == "ai" || k == "mcp" {
				p = k + "."
			}
			out = append(out, features(child, path+"."+k, p)...)
			continue
		}
		cat := categories[k]
		if cat == "" {
			cat = "other"
		}
		out = append(out, Feature{Category: cat, Key: prefix + k, Path: path + "." + k})
	}
	return out
}
