package main

import (
	"maps"
	"slices"
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
	Plugins   []Plugin       `json:"plugins,omitempty"`
	Obj       map[string]any `json:"obj,omitempty"`
}

// Plugin is a native plugin of Solo Enterprise for agentgateway: a library that
// parameters mount into the proxy (spec.plugins), or one instance of it that a
// policy turns on (spec.traffic.entPlugin). Of the configuration of an instance
// only the keys are kept: what a plugin is configured with is opaque and can be
// a credential.
type Plugin struct {
	Name string `json:"name"` // parameters: the library name; policy: the instance name
	Path string `json:"path"` // where in the object it is defined

	// A policy's instance.
	Library   string   `json:"library,omitempty"`   // the mounted library it runs: plugin, or else name
	Order     string   `json:"order,omitempty"`     // where in the request policy chain; empty is Late
	Condition string   `json:"condition,omitempty"` // the CEL condition of a conditional entry
	Fallback  bool     `json:"fallback,omitempty"`  // a conditional entry without a condition
	Config    []string `json:"config,omitempty"`    // the keys of its config, never the values
	// Gateways this instance applies to whose mounted plugins do not include its library.
	Unmounted []string `json:"unmounted,omitempty"`

	// A library that parameters mount.
	Image      string   `json:"image,omitempty"` // OCI image, mounted as an image volume
	PullPolicy string   `json:"pullPolicy,omitempty"`
	Secret     string   `json:"secret,omitempty"` // or a Secret in the proxy's namespace
	Keys       []string `json:"keys,omitempty"`   // the keys of that Secret it mounts; none means all
	// Gateways that mount this library and on which no policy turns it on.
	Unused []string `json:"unused,omitempty"`
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
	b.plugins()

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
	n.Plugins = append(mountedPlugins(spec), enabledPlugins(spec)...)
	return n
}

// mountedPlugins reads spec.plugins of a parameters object.
func mountedPlugins(spec map[string]any) []Plugin {
	list, _ := spec["plugins"].([]any)
	var out []Plugin
	for i, it := range list {
		m, _ := it.(map[string]any)
		name, _ := m["name"].(string)
		if name == "" {
			continue
		}
		p := Plugin{Name: name, Path: "spec.plugins." + strconv.Itoa(i)}
		p.Image, _ = m["image"].(string)
		p.PullPolicy, _ = m["pullPolicy"].(string)
		p.Secret, _, _ = unstructured.NestedString(m, "secretRef", "name")
		items, _, _ := unstructured.NestedSlice(m, "secretRef", "items")
		for _, it := range items {
			if k, _ := it.(map[string]any)["key"].(string); k != "" {
				p.Keys = append(p.Keys, k)
			}
		}
		out = append(out, p)
	}
	return out
}

type pluginEntry struct {
	m         map[string]any
	path      string
	condition string
	fallback  bool
}

func pluginEntries(spec map[string]any) []pluginEntry {
	traffic, _ := spec["traffic"].(map[string]any)
	ep, _ := traffic["entPlugin"].(map[string]any)
	if ep == nil {
		return nil
	}
	const at = "spec.traffic.entPlugin"
	cond, ok := ep["conditional"].([]any)
	if !ok {
		return []pluginEntry{{m: ep, path: at}}
	}
	var out []pluginEntry
	for i, c := range cond {
		m, _ := c.(map[string]any)
		pol, _ := m["policy"].(map[string]any)
		if pol == nil {
			continue
		}
		e := pluginEntry{m: pol, path: at + ".conditional." + strconv.Itoa(i) + ".policy"}
		e.condition, _ = m["condition"].(string)
		e.fallback = e.condition == ""
		out = append(out, e)
	}
	return out
}

// enabledPlugins reads the plugin instances that a policy turns on.
func enabledPlugins(spec map[string]any) []Plugin {
	var out []Plugin
	for _, e := range pluginEntries(spec) {
		name, _ := e.m["name"].(string)
		lib, _ := e.m["plugin"].(string)
		if lib == "" {
			lib = name
		}
		p := Plugin{Name: name, Path: e.path, Library: lib, Condition: e.condition, Fallback: e.fallback}
		p.Order, _ = e.m["order"].(string)
		if c, ok := e.m["config"].(map[string]any); ok {
			p.Config = slices.Sorted(maps.Keys(c))
		}
		out = append(out, p)
	}
	return out
}

func roleOf(kind string, spec map[string]any) string {
	_, hasRefs := spec["targetRefs"]
	if _, ok := spec["targetRef"]; ok {
		hasRefs = true // the older, singular form of policy attachment
	}
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
	case strings.HasSuffix(kind, "Backend") || strings.HasSuffix(kind, "Model") || kind == "InferencePool" || kind == "InferencePoolImport":
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
	b.linkDefaultGateways(n, spec)
	if n.Kind == "ReferenceGrant" {
		return // spec.to names what may be referred to, it is not a reference: see grants
	}
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

// linkDefaultGateways attaches a route that asks for the default gateways of a
// scope (spec.useDefaultGateways, Gateway API experimental) to every gateway that
// claims that scope (spec.defaultScope), in any namespace. Which listener takes it
// is decided as for a parentRef without a sectionName.
func (b *builder) linkDefaultGateways(n *Node, spec map[string]any) {
	scope, _ := spec["useDefaultGateways"].(string)
	if n.Role != "route" || scope == "" || scope == "None" {
		return
	}
	for _, g := range b.byKind["Gateway"] {
		gs, _, _ := unstructured.NestedString(g.Obj, "spec", "defaultScope")
		if !g.External && gs == scope {
			b.edge(Edge{From: n.ID, To: g.ID, Type: "parent", Path: "spec.useDefaultGateways", Note: "default gateway"})
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

// plugins checks native plugins per gateway. A gateway mounts the plugins of its
// own parameters and those of its class's, its own winning for the same name. A
// policy on the gateway, on one of its listener sets or on a route that reaches
// it turns an instance on, which runs a library by name. An instance whose
// library the gateway does not mount fails its requests (503); a mounted library
// that no instance runs is dead weight. Each policy instance gets a plugin edge
// to the parameters that mount its library.
//
// A gateway whose mounts are not all visible (it, its class or a parameters
// object was not captured) is not checked: what was not read is not wrong.
func (b *builder) plugins() {
	out, in := map[string][]Edge{}, map[string][]Edge{}
	for _, e := range b.edges {
		out[e.From] = append(out[e.From], e)
		in[e.To] = append(in[e.To], e)
	}
	// The gateways an object reaches: a gateway itself, the parent of a listener
	// set, the gateways of a route's parents, or of a route that delegates to it.
	var gatewaysOf func(n *Node, seen map[string]bool) []*Node
	gatewaysOf = func(n *Node, seen map[string]bool) []*Node {
		if seen[n.ID] {
			return nil
		}
		seen[n.ID] = true
		switch n.Role {
		case "gateway":
			return []*Node{n}
		case "listenerset", "route":
		default:
			return nil
		}
		var gws []*Node
		for _, e := range out[n.ID] {
			if e.Type == "parent" {
				gws = append(gws, gatewaysOf(b.nodes[e.To], seen)...)
			}
		}
		for _, e := range in[n.ID] {
			if e.Type == "backend" && b.nodes[e.From].Role == "route" {
				gws = append(gws, gatewaysOf(b.nodes[e.From], seen)...)
			}
		}
		return gws
	}
	type mount struct {
		params *Node
		i      int
	}
	// The libraries a gateway mounts, and whether every place they come from was read.
	mounts := func(g *Node) (map[string]mount, bool) {
		m := map[string]mount{}
		add := func(from *Node) bool {
			for _, e := range out[from.ID] {
				if e.Type != "params" {
					continue
				}
				p := b.nodes[e.To]
				if p.External {
					return false
				}
				for i, pl := range p.Plugins {
					if _, ok := m[pl.Name]; !ok && pl.Library == "" {
						m[pl.Name] = mount{p, i}
					}
				}
			}
			return true
		}
		if g.External || !add(g) {
			return nil, false
		}
		for _, e := range out[g.ID] {
			if e.Type == "class" && (b.nodes[e.To].External || !add(b.nodes[e.To])) {
				return nil, false
			}
		}
		return m, true
	}

	gws := slices.Clone(b.byKind["Gateway"])
	sort.Slice(gws, func(i, j int) bool { return gws[i].ID < gws[j].ID })
	used := map[*Node]map[string]bool{} // gateway -> libraries some instance runs
	var pols []*Node
	for _, n := range b.nodes {
		if n.Role == "policy" && !n.External && len(n.Plugins) > 0 {
			pols = append(pols, n)
		}
	}
	sort.Slice(pols, func(i, j int) bool { return pols[i].ID < pols[j].ID })
	for _, p := range pols {
		var reach []*Node
		seen := map[string]bool{}
		for _, e := range out[p.ID] {
			if e.Type == "target" {
				for _, g := range gatewaysOf(b.nodes[e.To], map[string]bool{}) {
					if !seen[g.ID] {
						seen[g.ID] = true
						reach = append(reach, g)
					}
				}
			}
		}
		sort.Slice(reach, func(i, j int) bool { return reach[i].ID < reach[j].ID })
		for _, g := range reach {
			m, ok := mounts(g)
			if !ok {
				continue
			}
			if used[g] == nil {
				used[g] = map[string]bool{}
			}
			for i := range p.Plugins {
				pl := &p.Plugins[i]
				mt, ok := m[pl.Library]
				if !ok {
					pl.Unmounted = append(pl.Unmounted, g.ID)
					continue
				}
				used[g][pl.Library] = true
				b.edge(Edge{From: p.ID, To: mt.params.ID, Type: "plugin", Section: pl.Library, Path: "spec.traffic.entPlugin"})
			}
		}
	}
	for _, g := range gws {
		m, ok := mounts(g)
		if !ok {
			continue
		}
		for _, lib := range slices.Sorted(maps.Keys(m)) {
			if !used[g][lib] {
				mt := m[lib]
				pl := &mt.params.Plugins[mt.i]
				pl.Unused = append(pl.Unused, g.ID)
			}
		}
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
			// A backendRef carries its own filters, which refer to objects too
			// (ExtensionRef, RequestMirror, ExternalAuth).
			if f, ok := t["filters"]; ok && typ == "backend" {
				b.walk(n, f, append(path, "filters"))
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
			// The config of a plugin is opaque: what looks like a reference in it is not one.
			if k == "config" && slices.Contains(path, "entPlugin") {
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
	case "extensionRef": // the same, in the v1alpha2 InferencePool
		if kind == "" && n.Kind == "InferencePool" {
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
	"sessionPersistence": "resilience", "retryConstraint": "resilience", // XBackendTrafficPolicy

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
