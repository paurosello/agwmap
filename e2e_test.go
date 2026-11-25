package main

// End-to-end tests: they build the binary and run it, the way a user would,
// against a small HTTP server that answers like a Kubernetes API server and
// serves the objects in testdata/. Nothing here calls the program's functions.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"sigs.k8s.io/yaml"
)

var bin string // the binary under test

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agwmap-e2e")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "kubectl-agwmap")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// Strings from testdata/ that an anonymized capture must not contain.
var leaks = []string{
	"acme", "T0000/B0000", // complex.yaml: every hostname, the project id, the webhook path
	"leak-corp", "leakcorp", "leak-ns", // leaks.yaml: hosts, e-mail, labels, gRPC service, kube context
	"leak-bearer", "leak-apikey", "sk-leak", "ghp_leak", "bGVhay11c2Vy", // inline credentials
	"leak-query-secret", "LEAKCLUSTERID", "8f3c1e2a", "0a1b2c3d", // URL query and path, GUIDs
	"123456789012", "2600:1f18", "203.0.113.77", // account id, addresses
	"jane.doe",                                           // the user named in a "forbidden" warning
	"search.hairpin", "tickets.hairpin", "repos.hairpin", // hairpin.yaml: Services that nothing names
}

// cluster is the fake API server.
type cluster struct {
	*httptest.Server
	kubeconfig string
	objects    int // how many objects a capture of all namespaces returns
	// When set, the user may only list in this namespace: every other list is refused.
	only string

	mu       sync.Mutex
	requests []string
}

type resource struct {
	kind       string
	namespaced bool
	items      []map[string]any
}

// forbidden is a resource that the server lists in discovery and refuses to list.
const forbidden = "xforbiddens"

func plural(kind string) string {
	k := strings.ToLower(kind)
	switch {
	case strings.HasSuffix(k, "y"):
		return k[:len(k)-1] + "ies"
	case strings.HasSuffix(k, "s"):
		return k + "es"
	}
	return k + "s"
}

func newCluster(t *testing.T) *cluster {
	t.Helper()
	// group -> version -> plural -> resource
	groups := map[string]map[string]map[string]*resource{}
	c := &cluster{}
	files, _ := filepath.Glob("testdata/*.yaml")
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, doc := range documents.Split(string(raw), -1) {
			var o map[string]any
			if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			apiVersion, _ := o["apiVersion"].(string)
			group, version, ok := strings.Cut(apiVersion, "/")
			if !ok {
				continue // empty document, or the core group (Namespace), which agwmap never reads
			}
			md := o["metadata"].(map[string]any)
			// What a real server adds and a capture must drop again.
			md["uid"] = "0f0f0f0f-0000-4000-8000-000000000000"
			md["resourceVersion"] = "1"
			md["managedFields"] = []any{map[string]any{"manager": "kubectl"}}
			kind := o["kind"].(string)
			if groups[group] == nil {
				groups[group] = map[string]map[string]*resource{}
			}
			if groups[group][version] == nil {
				groups[group][version] = map[string]*resource{}
			}
			r := groups[group][version][plural(kind)]
			if r == nil {
				_, namespaced := md["namespace"]
				r = &resource{kind: kind, namespaced: namespaced}
				groups[group][version][plural(kind)] = r
			}
			r.items = append(r.items, o)
			c.objects++
		}
	}
	gw := groups["gateway.networking.k8s.io"]
	gw["v1"][forbidden] = &resource{kind: "XForbidden", namespaced: true}
	// Like a real server, serve the v1 resources under the older version too.
	// ReferenceGrant stays in v1beta1 only, where the preferred version (v1)
	// does not have it.
	for name, r := range gw["v1"] {
		gw["v1beta1"][name] = r
	}

	preferred := func(group string) string {
		vs := slices.Sorted(func(yield func(string) bool) {
			for v := range groups[group] {
				if !yield(v) {
					return
				}
			}
		})
		if slices.Contains(vs, "v1") {
			return "v1"
		}
		return vs[0]
	}
	reply := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		c.mu.Lock()
		c.requests = append(c.requests, req.URL.Path)
		c.mu.Unlock()
		parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
		switch {
		case req.URL.Path == "/apis":
			list := []any{}
			// A group agwmap has no business with, to prove it is left alone.
			list = append(list, map[string]any{"name": "apps", "versions": []any{map[string]any{"groupVersion": "apps/v1"}},
				"preferredVersion": map[string]any{"groupVersion": "apps/v1"}})
			for g, versions := range groups {
				vs := []any{}
				for v := range versions {
					vs = append(vs, map[string]any{"groupVersion": g + "/" + v, "version": v})
				}
				list = append(list, map[string]any{"name": g, "versions": vs,
					"preferredVersion": map[string]any{"groupVersion": g + "/" + preferred(g)}})
			}
			reply(w, 200, map[string]any{"kind": "APIGroupList", "apiVersion": "v1", "groups": list})
		case len(parts) == 3 && parts[0] == "apis" && groups[parts[1]][parts[2]] != nil:
			rs := []any{}
			for name, r := range groups[parts[1]][parts[2]] {
				rs = append(rs,
					map[string]any{"name": name, "kind": r.kind, "namespaced": r.namespaced, "verbs": []any{"get", "list"}},
					map[string]any{"name": name + "/status", "kind": r.kind, "namespaced": r.namespaced, "verbs": []any{"get"}})
			}
			reply(w, 200, map[string]any{"kind": "APIResourceList", "apiVersion": "v1",
				"groupVersion": parts[1] + "/" + parts[2], "resources": rs})
		case (len(parts) == 4 || len(parts) == 6 && parts[3] == "namespaces") && parts[0] == "apis" &&
			groups[parts[1]][parts[2]][parts[len(parts)-1]] != nil:
			name := parts[len(parts)-1]
			if name == forbidden || (c.only != "" && (len(parts) == 4 || parts[4] != c.only)) {
				msg := fmt.Sprintf(`%s.%s is forbidden: User "jane.doe@leak-corp.com" cannot list resource %q in API group %q at the cluster scope`,
					name, parts[1], name, parts[1])
				reply(w, 403, map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure",
					"message": msg, "reason": "Forbidden", "code": 403})
				return
			}
			r := groups[parts[1]][parts[2]][name]
			items := []any{}
			for _, o := range r.items {
				if len(parts) == 6 && o["metadata"].(map[string]any)["namespace"] != parts[4] {
					continue
				}
				items = append(items, o)
			}
			reply(w, 200, map[string]any{"kind": r.kind + "List", "apiVersion": parts[1] + "/" + parts[2],
				"metadata": map[string]any{}, "items": items})
		default:
			reply(w, 404, map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "NotFound", "code": 404})
		}
	}))
	t.Cleanup(c.Close)

	c.kubeconfig = filepath.Join(t.TempDir(), "kubeconfig")
	cfg := fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: leak-corp-prod
clusters: [{name: c, cluster: {server: "%s"}}]
users: [{name: u, user: {token: t}}]
contexts: [{name: leak-corp-prod, context: {cluster: c, user: u}}]
`, c.URL)
	if err := os.WriteFile(c.kubeconfig, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return c
}

// agwmap runs the binary and returns stdout, stderr and the exit code.
func agwmap(t *testing.T, kubeconfig string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return stdout.String(), stderr.String(), code
}

// mustRun is agwmap for a call that must succeed.
func mustRun(t *testing.T, kubeconfig string, args ...string) string {
	t.Helper()
	stdout, stderr, code := agwmap(t, kubeconfig, args...)
	if code != 0 {
		t.Fatalf("agwmap %v: exit %d\n%s", args, code, stderr)
	}
	return stdout
}

// write puts content in a new temporary file with the given name.
func write(t *testing.T, name, content string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func noLeaks(t *testing.T, what, output string) {
	t.Helper()
	for _, s := range leaks {
		if i := strings.Index(output, s); i >= 0 {
			t.Errorf("%s contains %q: ...%s...", what, s, output[max(0, i-60):min(len(output), i+len(s)+40)])
		}
	}
}

// graphOf reads the graph back out of a rendered page.
func graphOf(t *testing.T, page string) Graph {
	t.Helper()
	const open, end = `<script id="data" type="application/json">`, `</script>`
	i := strings.Index(page, open)
	if i < 0 {
		t.Fatal("the page has no data block")
	}
	data := page[i+len(open):]
	var g Graph
	if err := json.Unmarshal([]byte(data[:strings.Index(data, end)]), &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// documents splits a multi-document YAML text.
var documents = regexp.MustCompile(`(?m)^---\s*$`)

// snapshotOf reads a snapshot file: the document that holds the meta, then one
// document per object.
func snapshotOf(t *testing.T, raw string) Snapshot {
	t.Helper()
	var s Snapshot
	for i, doc := range documents.Split(raw, -1) {
		var o map[string]any
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			s.Objects = append(s.Objects, o)
			continue
		}
		if o["kind"] != "agwmap-snapshot" {
			t.Fatalf("the first document is not the snapshot's: %v", o)
		}
		meta, _ := json.Marshal(o["meta"])
		if err := json.Unmarshal(meta, &s.Meta); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// shape is the graph without the objects: node ids and edges.
func shape(g Graph) []string {
	var out []string
	for _, n := range g.Nodes {
		out = append(out, "node "+n.ID+" "+n.Role)
	}
	for _, e := range g.Edges {
		out = append(out, fmt.Sprintf("edge %s -> %s %s section=%q crossNs=%v grant=%q", e.From, e.To, e.Type, e.Section, e.CrossNS, e.Grant))
	}
	sort.Strings(out)
	return out
}

// out runs agwmap with -o pointing at a new file with the given name (a .yaml
// name makes it a snapshot) and returns what was written.
func out(t *testing.T, kubeconfig, name string, args ...string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), name)
	mustRun(t, kubeconfig, append(args, "-o", file)...)
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestCaptureIsAnonymizedByDefault(t *testing.T) {
	c := newCluster(t)
	raw := out(t, c.kubeconfig, "snapshot.yaml")
	noLeaks(t, "the snapshot", raw)
	noLeaks(t, "the page", out(t, c.kubeconfig, "map.html"))

	s := snapshotOf(t, raw)
	if !s.Meta.Anonymized || s.Meta.Context != "cluster" {
		t.Errorf("meta = %+v, want anonymized with context \"cluster\"", s.Meta)
	}
	if len(s.Objects) != c.objects {
		t.Errorf("captured %d objects, the cluster has %d", len(s.Objects), c.objects)
	}
	// The listing that was refused is reported, without the user it was refused to.
	if len(s.Meta.Warnings) != 1 || !strings.Contains(s.Meta.Warnings[0], "could not list "+forbidden) ||
		!strings.Contains(s.Meta.Warnings[0], `User "<redacted>"`) {
		t.Errorf("warnings = %q", s.Meta.Warnings)
	}
	for _, o := range s.Objects {
		md := o["metadata"].(map[string]any)
		for k := range md {
			if !slices.Contains([]string{"name", "namespace", "labels", "annotations"}, k) {
				t.Errorf("%s %s keeps metadata.%s", o["kind"], md["name"], k)
			}
		}
	}
	// What makes the output useful survives: names, the shape of a URL, and the
	// hostname of a gateway's own proxy Service, which leads back into the gateway.
	for _, want := range []string{`name: llm-failover`, `/realms/p-`, `/protocol/openid-connect/certs`, `.example`,
		`host: edge.hairpin.svc.cluster.local`} {
		if !strings.Contains(raw, want) {
			t.Errorf("the snapshot does not contain %q", want)
		}
	}
}

func TestCaptureReadsOnlyGatewayGroups(t *testing.T) {
	c := newCluster(t)
	out(t, c.kubeconfig, "snapshot.yaml")
	for _, p := range c.requests {
		if p != "/apis" && !wantGroup(strings.Split(p, "/")[2]) {
			t.Errorf("agwmap requested %s", p)
		}
	}
}

func TestCaptureIsRepeatable(t *testing.T) {
	c := newCluster(t)
	stamp := regexp.MustCompile(`generatedAt: .*`)
	first := stamp.ReplaceAllString(out(t, c.kubeconfig, "a.yaml"), "")
	for i := 0; i < 5; i++ {
		if again := stamp.ReplaceAllString(out(t, c.kubeconfig, "b.yaml"), ""); again != first {
			t.Fatal("two captures of the same cluster differ")
		}
	}
}

func TestAnonymizeFalseKeepsRealValues(t *testing.T) {
	c := newCluster(t)
	raw := out(t, c.kubeconfig, "raw.yaml", "-anonymize=false")
	s := snapshotOf(t, raw)
	if s.Meta.Anonymized || s.Meta.Context != "leak-corp-prod" {
		t.Errorf("meta = %+v", s.Meta)
	}
	for _, want := range []string{"portal.leak-corp.com", "billing.leak-corp.com/cost-center"} {
		if !strings.Contains(raw, want) {
			t.Errorf("the snapshot does not contain %q", want)
		}
	}
	if len(s.Meta.Warnings) != 1 || !strings.Contains(s.Meta.Warnings[0], `User "jane.doe@leak-corp.com"`) {
		t.Errorf("warnings = %q", s.Meta.Warnings)
	}
}

// Anonymization must not change what the map shows: same objects, same edges.
func TestGraph(t *testing.T) {
	c := newCluster(t)
	g := graphOf(t, out(t, c.kubeconfig, "map.html"))
	real := graphOf(t, out(t, c.kubeconfig, "real.html", "-anonymize=false"))
	got, want := shape(g), shape(real)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the anonymized graph differs from the real one:\n%s", diff(want, got))
	}

	const gwapi, agw = "gateway.networking.k8s.io", "agentgateway.dev"
	edges := map[string]Edge{}
	for _, e := range g.Edges {
		edges[e.From+" -> "+e.To+" "+e.Type] = e
	}
	edge := func(from, to, typ string) (Edge, bool) {
		e, ok := edges[from+" -> "+to+" "+typ]
		if !ok {
			t.Errorf("no %s edge %s -> %s", typ, from, to)
		}
		return e, ok
	}
	// A parentRef with a sectionName.
	if e, ok := edge(gwapi+"/HTTPRoute/gwdemo-ai/chat", gwapi+"/Gateway/gwdemo-infra/ai", "parent"); ok && e.Section == "" {
		t.Errorf("the parent edge of chat has no section: %+v", e)
	}
	// A backendRef into another namespace, without and with a ReferenceGrant.
	if e, ok := edge(gwapi+"/HTTPRoute/gwdemo-apps/shop", "/Service/gwdemo-ai/metrics", "backend"); ok && (!e.CrossNS || e.Grant != "") {
		t.Errorf("shop -> metrics: %+v, want cross-namespace without a grant", e)
	}
	if e, ok := edge(gwapi+"/HTTPRoute/gwdemo-mcp/mcp", "/Service/gwdemo-apps/status", "backend"); ok && (!e.CrossNS || e.Grant == "") {
		t.Errorf("mcp -> status: %+v, want cross-namespace with a grant", e)
	}
	// The other Gateway API kinds: a ListenerSet and a route attached to it, the
	// TLS, TCP and UDP routes, and the experimental kinds.
	const gwx = "gateway.networking.x-k8s.io"
	edge(gwapi+"/ListenerSet/l4/extra", gwapi+"/Gateway/l4/edge", "parent")
	if e, ok := edge(gwapi+"/HTTPRoute/l4/admin", gwapi+"/ListenerSet/l4/extra", "parent"); ok && e.Section != "admin" {
		t.Errorf("admin -> listener set: %+v, want section admin", e)
	}
	if e, ok := edge(gwapi+"/TLSRoute/l4/postgres", "/Service/l4-other/postgres", "backend"); ok && (!e.CrossNS || e.Grant == "") {
		t.Errorf("postgres: %+v, want cross-namespace with a grant", e)
	}
	if e, ok := edge(gwapi+"/TCPRoute/l4/queue", "/Service/l4-other/queue-b", "backend"); ok && (!e.CrossNS || e.Grant != "") {
		t.Errorf("queue -> queue-b: %+v, want cross-namespace without a grant", e)
	}
	edge(gwapi+"/TCPRoute/l4/queue", gwapi+"/Gateway/l4/edge", "parent")
	edge(gwapi+"/UDPRoute/l4/dns", "/Service/l4/dns", "backend")
	edge(gwapi+"/HTTPRoute/l4/admin", gwx+"/XBackend/l4/partner", "backend")
	// References inside filters: of a rule (a mirror), and of one backendRef.
	if e, ok := edge(gwapi+"/HTTPRoute/l4/admin", "/Service/l4-other/shadow", "backend"); ok && !e.CrossNS {
		t.Errorf("the mirror backend is not marked cross-namespace: %+v", e)
	}
	edge(gwapi+"/HTTPRoute/l4/admin", gwx+"/XBackend/l4/partner", "ref")
	edge(gwx+"/XBackendTrafficPolicy/l4/queue-retries", "/Service/l4/queue-a", "target")
	edge(gwx+"/XTelemetryPolicy/l4/edge-tracing", gwapi+"/Gateway/l4/edge", "target")
	roles := map[string]string{}
	for _, n := range g.Nodes {
		roles[n.ID] = n.Role
	}
	for id, want := range map[string]string{
		gwapi + "/ListenerSet/l4/extra": "listenerset", gwapi + "/TLSRoute/l4/postgres": "route",
		gwapi + "/TCPRoute/l4/queue": "route", gwapi + "/UDPRoute/l4/dns": "route",
		gwx + "/XBackend/l4/partner": "backend", gwx + "/XBackendTrafficPolicy/l4/queue-retries": "policy",
		gwx + "/XTelemetryPolicy/l4/edge-tracing": "policy",
	} {
		if roles[id] != want {
			t.Errorf("%s has role %q, want %q", id, roles[id], want)
		}
	}
	// A policy that selects its target by label, where the label value is a hostname.
	edge(agw+"/AgentgatewayPolicy/leak-test/leaks", gwapi+"/Gateway/leak-test/leaks", "target")
	// A reference to something that was not captured becomes a stub.
	stubs := 0
	for _, n := range g.Nodes {
		if n.External && n.Kind == "Secret" {
			stubs++
		}
	}
	if stubs == 0 {
		t.Error("no Secret stub: references to uncaptured objects are lost")
	}
}

// Native plugins (plugins.yaml): what parameters mount, what policies turn on, and
// the two mistakes between them. Of a plugin's config only the keys are kept.
func TestPlugins(t *testing.T) {
	c := newCluster(t)
	page := out(t, c.kubeconfig, "map.html")
	g := graphOf(t, page)
	const (
		params = "enterpriseagentgateway.solo.io/EnterpriseAgentgatewayParameters/plug/"
		policy = "enterpriseagentgateway.solo.io/EnterpriseAgentgatewayPolicy/plug/"
		gw     = "gateway.networking.k8s.io/Gateway/plug/plugged"
	)
	plugins := map[string][]Plugin{}
	for _, n := range g.Nodes {
		if strings.Contains(n.ID, "plugin-config-only") {
			t.Errorf("a reference inside a plugin's config became a node: %s", n.ID)
		}
		plugins[n.ID] = n.Plugins
	}
	same := func(what string, got, want []Plugin) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %+v\n want %+v", what, got, want)
		}
	}
	// The image's registry is a hostname, and anonymized as one.
	image := func(p Plugin) string { return p.Image[strings.Index(p.Image, "/"):] }
	gwp := plugins[params+"plug-gw"]
	if len(gwp) != 3 || image(gwp[0]) != "/gw-plugins/authz:v2" || image(gwp[2]) != "/gw-plugins/spare:v1" {
		t.Fatalf("the gateway's parameters mount %+v", gwp)
	}
	gwp[0].Image, gwp[2].Image = "", ""
	same("the gateway's parameters", gwp, []Plugin{
		{Name: "authz", Path: "spec.plugins.0", PullPolicy: "IfNotPresent"},
		{Name: "hello", Path: "spec.plugins.1", Secret: "hello-lib", Keys: []string{"libhello.so"}},
		{Name: "spare", Path: "spec.plugins.2", Unused: []string{gw}}, // nothing turns it on
	})
	// The class mounts authz too, but the gateway's own entry wins: not reported as unused.
	for _, p := range plugins[params+"plug-defaults"] {
		if len(p.Unused) != 0 {
			t.Errorf("class plugin %s reported unused: %+v", p.Name, p)
		}
	}
	same("authz", plugins[policy+"authz"], []Plugin{{Name: "authz", Path: "spec.traffic.entPlugin", Library: "authz",
		Order: "PostAuthorization", Config: []string{"policy", "resultHeader", "upstream"}}})
	same("tenant-hello", plugins[policy+"tenant-hello"], []Plugin{
		{Name: "tenant-hello", Path: "spec.traffic.entPlugin.conditional.0.policy", Library: "hello",
			Condition: `request.headers["x-tenant"] == "blue"`, Config: []string{"greeting"}},
		{Name: "tenant-hello", Path: "spec.traffic.entPlugin.conditional.1.policy", Library: "hello", Fallback: true},
	})
	// A route policy whose library no parameters of its gateway mount.
	same("ghost", plugins[policy+"ghost"], []Plugin{{Name: "ghost", Path: "spec.traffic.entPlugin", Library: "ghost",
		Order: "Late", Config: []string{"mode"}, Unmounted: []string{gw}}})

	edges := map[string]bool{}
	for _, e := range g.Edges {
		if e.Type == "plugin" {
			edges[e.From+" -> "+e.To+" #"+e.Section] = true
		}
	}
	want := map[string]bool{
		policy + "authz -> " + params + "plug-gw #authz":        true, // the gateway's, not the class's
		policy + "stamp -> " + params + "plug-defaults #stamp":  true, // mounted by the class
		policy + "tenant-hello -> " + params + "plug-gw #hello": true, // through the route
	}
	if !reflect.DeepEqual(edges, want) {
		t.Errorf("plugin edges = %v, want %v", edges, want)
	}

	// The config's values are gone from what is shared, its keys stay.
	snap := out(t, c.kubeconfig, "snapshot.yaml")
	for _, s := range []string{"leak-corp-admins", "x-authz-result", "plugin-config-only", "ghostly-mode-value"} {
		if strings.Contains(snap, s) || strings.Contains(page, s) {
			t.Errorf("the capture keeps %q of a plugin's config", s)
		}
	}
	if !strings.Contains(snap, "resultHeader: <redacted>") {
		t.Error("the keys of a plugin's config are lost")
	}
	// Shapes the API refuses, which a manifest given to -from can still have: an
	// inline config next to conditional, a config that is not an object.
	odd := write(t, "odd.yaml", `apiVersion: enterpriseagentgateway.solo.io/v1alpha1
kind: EnterpriseAgentgatewayPolicy
metadata: {name: odd, namespace: plug}
spec:
  targetRefs: [{group: gateway.networking.k8s.io, kind: Gateway, name: plugged}]
  traffic:
    entPlugin:
      config: {token: odd-inline-value}
      conditional:
      - policy: {name: odd, config: odd-string-value}
`)
	for _, f := range []string{"odd.yaml", "odd.html"} {
		got := out(t, "/nonexistent", f, "-from", odd)
		if strings.Contains(got, "odd-inline-value") || strings.Contains(got, "odd-string-value") {
			t.Errorf("%s keeps a value of a plugin's config", f)
		}
	}
	// With -anonymize=false the object is kept as it is, and the graph still holds only the keys.
	real := graphOf(t, out(t, c.kubeconfig, "real.html", "-anonymize=false"))
	for _, n := range real.Nodes {
		if n.ID == policy+"ghost" && (len(n.Plugins) != 1 || !reflect.DeepEqual(n.Plugins[0].Config, []string{"mode"})) {
			t.Errorf("ghost, not anonymized: %+v", n.Plugins)
		}
	}
}

// fields.yaml, rendered with -from: the edges and settings of fields that change
// where a request goes.
func fieldsGraph(t *testing.T) (map[string]*Node, map[string]Edge) {
	t.Helper()
	file, err := filepath.Abs("testdata/fields.yaml")
	if err != nil {
		t.Fatal(err)
	}
	g := graphOf(t, mustRun(t, "/nonexistent", "-from", file, "-o", "-"))
	nodes, edges := map[string]*Node{}, map[string]Edge{}
	for _, n := range g.Nodes {
		nodes[n.ID] = n
	}
	for _, e := range g.Edges {
		edges[e.From+" -> "+e.To+" "+e.Type] = e
	}
	return nodes, edges
}

func TestDefaultGateways(t *testing.T) {
	_, edges := fieldsGraph(t)
	const gw, rt = "gateway.networking.k8s.io/Gateway/fld-infra/", "gateway.networking.k8s.io/HTTPRoute/fld/"
	if e, ok := edges[rt+"menu -> "+gw+"commons parent"]; !ok || e.Path != "spec.useDefaultGateways" {
		t.Errorf("menu asks for the default gateways and is not attached to commons: %+v", e)
	}
	for _, k := range []string{rt + "menu -> " + gw + "closed parent", rt + "quiet -> " + gw + "commons parent"} {
		if _, ok := edges[k]; ok {
			t.Errorf("unexpected edge %s", k)
		}
	}
}

func diff(want, got []string) string {
	var b strings.Builder
	for _, s := range want {
		if !slices.Contains(got, s) {
			b.WriteString("- " + s + "\n")
		}
	}
	for _, s := range got {
		if !slices.Contains(want, s) {
			b.WriteString("+ " + s + "\n")
		}
	}
	return b.String()
}

func TestFromSnapshot(t *testing.T) {
	c := newCluster(t)
	page := out(t, c.kubeconfig, "map.html")
	anon := write(t, "anon.yaml", out(t, c.kubeconfig, "anon.yaml"))
	raw := write(t, "raw.yaml", out(t, c.kubeconfig, "raw.yaml", "-anonymize=false"))
	const none = "/nonexistent" // -from must not need a cluster

	// A snapshot renders to the page a direct capture gives.
	fromAnon := mustRun(t, none, "-from", anon, "-o", "-")
	if got, want := shape(graphOf(t, fromAnon)), shape(graphOf(t, page)); !reflect.DeepEqual(got, want) {
		t.Errorf("the page from a snapshot differs from the direct one:\n%s", diff(want, got))
	}
	// A snapshot with the real values is anonymized when rendered ...
	fromRaw := mustRun(t, none, "-from", raw, "-o", "-")
	noLeaks(t, "the page from a raw snapshot", fromRaw)
	if !graphOf(t, fromRaw).Meta.Anonymized {
		t.Error("the page from a raw snapshot is not marked anonymized")
	}
	noLeaks(t, "the snapshot from a raw snapshot", out(t, none, "again.yaml", "-from", raw))
	// ... unless asked not to.
	if kept := mustRun(t, none, "-from", raw, "-anonymize=false", "-o", "-"); !strings.Contains(kept, "portal.leak-corp.com") {
		t.Error("-from with -anonymize=false lost the real values")
	}
	// Anonymizing later gives what anonymizing at capture time gives.
	a, b := snapshotOf(t, out(t, none, "later.yaml", "-from", raw)), snapshotOf(t, out(t, c.kubeconfig, "now.yaml"))
	if !reflect.DeepEqual(a.Objects, b.Objects) {
		t.Error("a raw snapshot anonymized by -from differs from an anonymized capture")
	}
}

// -from also takes a manifest that agwmap did not write.
func TestFromManifest(t *testing.T) {
	const none = "/nonexistent" // no cluster is needed
	demo, err := filepath.Abs("testdata/demo.yaml")
	if err != nil {
		t.Fatal(err)
	}
	page := mustRun(t, none, "-from", demo, "-o", "-")
	if strings.Contains(page, "demo-company") {
		t.Error("the page from a manifest is not anonymized")
	}
	g := graphOf(t, page)
	if !g.Meta.Anonymized || g.Meta.GeneratedAt == "" {
		t.Errorf("meta = %+v", g.Meta)
	}
	const gwapi = "gateway.networking.k8s.io"
	found := map[string]bool{}
	for _, e := range g.Edges {
		found[e.From+" -> "+e.To+" "+e.Type] = e.Type != "backend" || !e.CrossNS || e.Grant != ""
	}
	for _, want := range []string{
		gwapi + "/HTTPRoute/agwmap-demo/shop -> " + gwapi + "/Gateway/agwmap-demo/demo parent",
		gwapi + "/HTTPRoute/agwmap-demo/shop -> /Service/agwmap-demo-reviews/reviews backend", // with its grant
		gwapi + "/HTTPRoute/agwmap-demo/chat -> agentgateway.dev/AgentgatewayBackend/agwmap-demo/openai backend",
	} {
		if !found[want] {
			t.Errorf("no edge %s", want)
		}
	}

	// A List, as kubectl get -o yaml prints it, with what a server adds; and
	// kinds outside the gateway API groups, which are left alone.
	list := write(t, "list.yaml", `apiVersion: v1
kind: List
items:
- apiVersion: gateway.networking.k8s.io/v1
  kind: Gateway
  metadata: {name: edge, namespace: listed, uid: 0f0f0f0f-0000-4000-8000-000000000000, resourceVersion: "7"}
  spec: {gatewayClassName: agentgateway, listeners: [{name: http, port: 80, protocol: HTTP}]}
---
apiVersion: v1
kind: Secret
metadata: {name: token, namespace: listed}
stringData: {token: sk-leak-from-a-manifest}
`)
	raw := out(t, none, "list.yaml", "-from", list, "-anonymize=false")
	s := snapshotOf(t, raw)
	if len(s.Objects) != 1 || s.Objects[0]["kind"] != "Gateway" || s.Meta.Context != "list.yaml" {
		t.Errorf("from a List: meta %+v, objects %v", s.Meta, s.Objects)
	}
	for _, gone := range []string{"sk-leak-from-a-manifest", "uid", "resourceVersion"} {
		if strings.Contains(raw, gone) {
			t.Errorf("the snapshot of a manifest contains %q", gone)
		}
	}
}

// Without -o the page goes to a new file in the temp folder, and no browser is
// opened when stderr is not a terminal.
// -from also takes the URL of a file.
func TestFromURL(t *testing.T) {
	const none = "/nonexistent" // no cluster is needed
	demo, err := filepath.Abs("testdata/demo.yaml")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/raw/demo.yaml":
			http.ServeFile(w, r, demo)
		case "/blob/demo.yaml":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, "<html></html>")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// The query stands for the token in the link to a file of a private repository.
	stdout, stderr, code := agwmap(t, none, "-from", srv.URL+"/raw/demo.yaml?token=tok-secret", "-anonymize=false", "-o", "-")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	g := graphOf(t, stdout)
	if got, want := shape(g), shape(graphOf(t, mustRun(t, none, "-from", demo, "-anonymize=false", "-o", "-"))); !reflect.DeepEqual(got, want) {
		t.Errorf("the page from a URL differs from the one from the file:\n%s", diff(want, got))
	}
	if g.Meta.Context != "demo.yaml" {
		t.Errorf("context = %q, want the file name", g.Meta.Context)
	}
	if strings.Contains(stdout+stderr, "tok-secret") {
		t.Error("the query of the URL is in the output")
	}

	for path, want := range map[string]string{
		"/missing.yaml?token=tok-secret":   "404",
		"/blob/demo.yaml?token=tok-secret": "use the link to the raw file",
	} {
		_, stderr, code := agwmap(t, none, "-from", srv.URL+path, "-o", "-")
		if code != 1 || !strings.Contains(stderr, want) || strings.Contains(stderr, "tok-secret") {
			t.Errorf("%s: exit %d, stderr %q", path, code, stderr)
		}
	}
	// Nothing listens here.
	srv.Close()
	if _, stderr, code := agwmap(t, none, "-from", srv.URL+"/raw/demo.yaml?token=tok-secret", "-o", "-"); code != 1 || strings.Contains(stderr, "tok-secret") {
		t.Errorf("unreachable: exit %d, stderr %q", code, stderr)
	}
}

// -from also takes a folder and reads the YAML files under it.
func TestFromFolder(t *testing.T) {
	const none = "/nonexistent" // no cluster is needed
	const gateway = `apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: edge, namespace: shop}
spec:
  gatewayClassName: agentgateway
  listeners: [{name: http, port: %d, protocol: HTTP}]
`
	dir := filepath.Join(t.TempDir(), "manifests")
	for name, content := range map[string]string{
		"base/gateway.yaml": fmt.Sprintf(gateway, 80),
		"base/routes/cart.yml": `apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: cart, namespace: shop}
spec:
  parentRefs: [{name: edge}]
  rules: [{backendRefs: [{name: cart, port: 8080}]}]
`,
		"overlay/gateway.yaml":   fmt.Sprintf(gateway, 8080), // the same Gateway again
		"chart/templates/x.yaml": "kind: Gateway\nmetadata:\n  name: {{ .Release.Name }}\n{{- if .Values.on }}\n",
		"chart/values.yaml":      "replicas: 2\n",
		".git/hidden.yaml":       strings.Replace(fmt.Sprintf(gateway, 80), "edge", "hidden", 1),
		"notes.txt":              strings.Replace(fmt.Sprintf(gateway, 80), "edge", "not-yaml", 1),
	} {
		file := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	s := snapshotOf(t, out(t, none, "folder.yaml", "-from", dir, "-anonymize=false"))
	var got []string
	for _, o := range s.Objects {
		meta, _ := o["metadata"].(map[string]any)
		got = append(got, fmt.Sprint(o["kind"], "/", meta["name"]))
	}
	sort.Strings(got)
	if want := []string{"Gateway/edge", "HTTPRoute/cart"}; !reflect.DeepEqual(got, want) {
		t.Errorf("objects = %v, want %v", got, want)
	}
	if s.Meta.Context != "manifests" {
		t.Errorf("context = %q, want the folder name", s.Meta.Context)
	}
	// The first definition of the Gateway is the one kept.
	if raw, _ := yaml.Marshal(s.Objects); strings.Contains(string(raw), "8080\n    protocol") || !strings.Contains(string(raw), "port: 80\n") {
		t.Errorf("the kept Gateway is not the first one:\n%s", raw)
	}
	warnings := strings.Join(s.Meta.Warnings, "\n")
	if len(s.Meta.Warnings) != 2 ||
		!strings.Contains(warnings, filepath.Join("chart", "templates", "x.yaml")+" was skipped") ||
		!strings.Contains(warnings, "Gateway shop/edge is already defined in "+filepath.Join("base", "gateway.yaml")) {
		t.Errorf("warnings = %q", s.Meta.Warnings)
	}
	// Through a link to the folder, and anonymized: the warnings name the files
	// by their path inside the folder and quote nothing of them.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	page := mustRun(t, none, "-from", link, "-o", "-")
	g := graphOf(t, page)
	if !g.Meta.Anonymized || g.Meta.Context != "cluster" || !reflect.DeepEqual(g.Meta.Warnings, s.Meta.Warnings) {
		t.Errorf("page meta = %+v", g.Meta)
	}
	if strings.Contains(page, "Release.Name") || strings.Contains(page, filepath.Dir(dir)) {
		t.Error("the page holds the text of a skipped file or the path of the folder")
	}

	empty := filepath.Join(dir, "chart")
	if _, stderr, code := agwmap(t, none, "-from", empty, "-o", "-"); code != 1 || !strings.Contains(stderr, "no object of the gateway API groups") {
		t.Errorf("a folder without gateway objects: exit %d, stderr %q", code, stderr)
	}
}

func TestDefaultOutput(t *testing.T) {
	demo, err := filepath.Abs("testdata/demo.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	cmd := exec.Command(bin, "-from", demo)
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
	stderr, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("agwmap: %v\n%s", err, stderr)
	}
	pages, _ := filepath.Glob(filepath.Join(tmp, "agwmap-*.html"))
	if len(pages) != 1 || !strings.Contains(string(stderr), pages[0]) {
		t.Fatalf("pages in the temp folder: %v, stderr %q", pages, stderr)
	}
	if st, err := os.Stat(pages[0]); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("the page is not private to its owner: %v %v", st.Mode(), err)
	}
	raw, err := os.ReadFile(pages[0])
	if err != nil {
		t.Fatal(err)
	}
	graphOf(t, string(raw))
	if strings.Contains(string(stderr), "browser") {
		t.Errorf("stderr is not a terminal, but: %q", stderr)
	}
	if left, _ := os.ReadDir(cmd.Dir); len(left) != 0 {
		t.Errorf("files left in the working directory: %v", left)
	}
}

func TestNamespace(t *testing.T) {
	c := newCluster(t)
	s := snapshotOf(t, out(t, c.kubeconfig, "ns.yaml", "-n", "leak-test"))
	kinds := map[string]int{}
	for _, o := range s.Objects {
		md := o["metadata"].(map[string]any)
		if ns, ok := md["namespace"]; ok && ns != "leak-test" {
			t.Errorf("%s %s/%s is outside the namespace", o["kind"], ns, md["name"])
		}
		kinds[o["kind"].(string)]++
	}
	// The four objects of leaks.yaml, and what is cluster-scoped.
	want := map[string]int{"Gateway": 1, "HTTPRoute": 1, "GRPCRoute": 1, "AgentgatewayPolicy": 1,
		"GatewayClass": 2, "EnterpriseAgentgatewayRoleBinding": 1, "XMesh": 1}
	if !reflect.DeepEqual(kinds, want) {
		t.Errorf("kinds = %v, want %v", kinds, want)
	}
}

// Someone who may only read their own namespace, under a gateway that a platform
// team runs in another one: the gateway is known only as what their objects name.
func TestNamespaceOfATenant(t *testing.T) {
	c := newCluster(t)
	c.only = "tenant-app"

	// Without -n nothing is readable, and the output says what to do about it.
	_, stderr, code := agwmap(t, c.kubeconfig, "-o", filepath.Join(t.TempDir(), "all.yaml"))
	if code != 0 || !strings.Contains(stderr, "use -n NAMESPACE") {
		t.Errorf("all namespaces: exit %d, stderr without the hint to use -n:\n%s", code, stderr)
	}

	page := out(t, c.kubeconfig, "tenant.html", "-n", "tenant-app")
	g := graphOf(t, page)
	if g.Meta.Namespace != "tenant-app" {
		t.Errorf("meta.namespace = %q: the page cannot tell that only one namespace was read", g.Meta.Namespace)
	}
	for _, w := range g.Meta.Warnings {
		if strings.Contains(w, "use -n") {
			t.Errorf("the hint to use -n is given with -n: %q", w)
		}
	}
	nodes := map[string]*Node{}
	var captured []string
	for _, n := range g.Nodes {
		nodes[n.ID] = n
		if !n.External {
			captured = append(captured, n.Kind+" "+n.Namespace+"/"+n.Name)
		}
	}
	// Every object of the namespace, and none of the gateway's or of another team's.
	if want := []string{"AgentgatewayBackend tenant-app/spare", "AgentgatewayPolicy tenant-app/orders-timeout",
		"HTTPRoute tenant-app/catalog", "HTTPRoute tenant-app/lost", "HTTPRoute tenant-app/orders", "HTTPRoute tenant-app/reports",
		"ListenerSet tenant-app/app", "ReferenceGrant tenant-app/platform-to-reports"}; !reflect.DeepEqual(captured, want) {
		t.Errorf("captured %v, want %v", captured, want)
	}
	const gwapi = "gateway.networking.k8s.io"
	gw := nodes[gwapi+"/Gateway/tenant-platform/shared"]
	if gw == nil || !gw.External {
		t.Fatalf("the platform's gateway: %+v, want a stub", gw)
	}
	// What the page draws the flows from: the listener set and the routes that name the gateway.
	parents, targeted := map[string]string{}, false
	for _, e := range g.Edges {
		targeted = targeted || (e.Type == "target" && e.From == "agentgateway.dev/AgentgatewayPolicy/tenant-app/orders-timeout" && e.To == gwapi+"/HTTPRoute/tenant-app/orders")
		if e.Type == "parent" {
			parents[e.From+" -> "+e.To] = e.Section
		}
		// The grant for search is in tenant-data, which was not read.
		if strings.HasSuffix(e.To, "/Service/tenant-data/search") && (!e.CrossNS || e.Grant != "") {
			t.Errorf("catalog -> search: %+v, want cross-namespace without a grant", e)
		}
	}
	want := map[string]string{
		gwapi + "/ListenerSet/tenant-app/app -> " + gw.ID:                                  "",
		gwapi + "/HTTPRoute/tenant-app/catalog -> " + gw.ID:                                "https",
		gwapi + "/HTTPRoute/tenant-app/lost -> " + gw.ID:                                   "internal",
		gwapi + "/HTTPRoute/tenant-app/orders -> " + gwapi + "/ListenerSet/tenant-app/app": "orders",
	}
	if !reflect.DeepEqual(parents, want) {
		t.Errorf("parent edges = %v, want %v", parents, want)
	}
	if !targeted {
		t.Error("the policy orders-timeout is not attached to the route orders")
	}
	// The route's status is all the tenant sees of the gateway: it must survive the capture.
	if route := nodes[gwapi+"/HTTPRoute/tenant-app/catalog"]; route == nil {
		t.Error("the route catalog was not captured")
	} else if status, _ := route.Obj["status"].(map[string]any); len(status) == 0 || status["parents"] == nil {
		t.Errorf("status of catalog = %v, want its parents", status)
	}

	// A snapshot carries the namespace, so the page rendered from it is the same view.
	snap := out(t, c.kubeconfig, "tenant.yaml", "-n", "tenant-app")
	from := graphOf(t, mustRun(t, c.kubeconfig, "-from", write(t, "tenant.yaml", snap), "-o", "-"))
	if from.Meta.Namespace != "tenant-app" || !reflect.DeepEqual(shape(from), shape(g)) {
		t.Errorf("the page from the snapshot differs: namespace %q\n%s", from.Meta.Namespace, diff(shape(g), shape(from)))
	}
}

func TestUsageErrors(t *testing.T) {
	c := newCluster(t)
	// A stray argument is an error, not a capture of the live cluster.
	if _, stderr, code := agwmap(t, c.kubeconfig, "snapshot.yaml"); code != 2 || !strings.Contains(stderr, "-from snapshot.yaml") {
		t.Errorf("stray argument: exit %d, stderr %q", code, stderr)
	}
	if len(c.requests) != 0 {
		t.Errorf("a stray argument still reached the cluster: %v", c.requests)
	}
	nothing := write(t, "x.yaml", "apiVersion: v1\nkind: Secret\nmetadata: {name: x}\n")
	if _, stderr, code := agwmap(t, c.kubeconfig, "-from", nothing); code != 1 || !strings.Contains(stderr, "no object of the gateway API groups") {
		t.Errorf("wrong file: exit %d, stderr %q", code, stderr)
	}
	if _, stderr, code := agwmap(t, "/nonexistent", "-o", "-"); code != 1 || stderr == "" {
		t.Errorf("no cluster: exit %d, stderr %q", code, stderr)
	}
	if got := mustRun(t, c.kubeconfig, "-version"); strings.TrimSpace(got) == "" {
		t.Error("-version prints nothing")
	}
}
