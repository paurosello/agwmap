package main

import (
	"fmt"
	"maps"
	"net"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Anonymization replaces values that identify the owner of a cluster with
// stable pseudonyms, so that the same host is the same pseudonym everywhere
// and the relations between objects stay readable.
//
// Replaced:
//   - hostnames, wherever they appear, and IPv4 and IPv6 addresses
//   - URLs: the host, every path segment that is not a well-known word
//     (/v1, /.well-known, /oauth2 ...), and the whole query and fragment
//   - e-mail addresses, GUIDs, AWS account ids and resource names in ARNs,
//     Google Cloud project ids, client/tenant/account/project id fields
//   - inline credentials: values of secret-like keys and headers, bearer and
//     basic tokens, JWTs, API keys with a known prefix, PEM blocks
//   - gRPC service names in route matches
//   - label and annotation values, by the same rules
//
// In-cluster service hostnames are replaced too (svc-1.ns-1.svc.cluster.local).
//
// Kept: the names and namespaces of the objects themselves and the names of the
// objects they refer to (Secrets, Services, models), which the map needs to
// make sense; label and annotation keys; and free text such as descriptions,
// CEL expressions, model names, header names and paths in route matches, apart
// from the values above found inside it. Which labels and annotations survive
// at all is decided in minimize.go.

const redacted = "<redacted>"

var (
	urlRe        = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)([^/\s?#"'<>]+)([^\s"'<>]*)`)
	bareHostRe   = regexp.MustCompile(`^(\*\.)?([a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?\.)+[a-zA-Z][a-zA-Z0-9-]*(:\d+)?$`)
	dottedRe     = regexp.MustCompile(`(\*\.)?[a-zA-Z0-9-]+(\.[a-zA-Z0-9-]+)+`)
	ipRe         = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	ip6Re        = regexp.MustCompile(`[0-9a-fA-F]*:[0-9a-fA-F:]*:[0-9a-fA-F:]*`)
	emailRe      = regexp.MustCompile(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`)
	arnRe        = regexp.MustCompile(`(arn:aws[a-z-]*:[^:\s]*:[^:\s]*:)(\d{12})(:[^\s"',]+)?`)
	gcpProjectRe = regexp.MustCompile(`\bprojects/[a-z][a-z0-9-]{4,29}\b`)
	guidRe       = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	clusterSvcRe = regexp.MustCompile(`^([^.]+)\.([^.]+)\.svc(\.cluster\.local)?$`)
	svcInTextRe  = regexp.MustCompile(`\b[a-z0-9][a-z0-9-]*\.[a-z0-9][a-z0-9-]*\.svc(\.cluster\.local)?\b`)
	pseudoSvcRe  = regexp.MustCompile(`^svc-\d+\.ns-\d+\.svc`)
	jwtRe        = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*`)
	// A bearer or basic token, or an API key with a vendor prefix, anywhere in a string.
	credentialRe = regexp.MustCompile(`(?i:\b(bearer|basic)\s+[a-z0-9._~+/=-]{16,})` +
		`|\b(sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}` +
		`|xox[abprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{30,}|glpat-[A-Za-z0-9_-]{16,})`)

	hostKeyRe   = regexp.MustCompile(`(?i)(host|domain|sni|authority|issuer|audience|origin|address|endpoint|server|url|uri)`)
	secretKeyRe = regexp.MustCompile(`(?i)(secret|password|passwd|token|api[_-]?key|credential|private[_-]?key|authorization|cookie)$`)
	// "inline" holds a credential or key under these parents (auth.key.inline, jwks.inline),
	// and plain configuration elsewhere (WAF directives).
	inlineParentRe = regexp.MustCompile(`(?i)(auth|key|secret|credential|tls|cert|password|jwks|token)`)
	idKeyRe        = regexp.MustCompile(`(?i)^(client_?id|project_?id|account_?id|tenant_?id|subscription_?id|organization_?id|resourceName|projectName)$`)
	// {name: <header>, value: ...} pairs whose value is a credential or a host.
	secretHeaderRe = regexp.MustCompile(`(?i)^(proxy-)?authorization$|cookie$|api[_-]?key|secret|password|^token$|^x-.*-token$`)
	hostHeaderRe   = regexp.MustCompile(`(?i)^(host|:authority|x-forwarded-host)$`)

	// Keys whose values are Kubernetes identifiers or enums, never hosts. A value
	// that is not one after all (a sentence, a URL) is still anonymized as text.
	plainKeys = map[string]bool{
		"name": true, "namespace": true, "kind": true, "group": true, "apiVersion": true,
		"sectionName": true, "type": true, "gatewayClassName": true, "controllerName": true,
		"reason": true, "status": true,
	}

	// URL path segments that say what an endpoint is, not whose it is.
	plainSegments = map[string]bool{}
	// Top-level domains a hostname is recognised by when it sits in free text.
	// Short on purpose: words like "host", "name" or "id" follow a dot in CEL.
	textTLDs = map[string]bool{}
	// Domains of the APIs themselves, which show up as groups and controller names.
	apiDomains = []string{"k8s.io", "kubernetes.io", "kgateway.dev", "agentgateway.dev", "solo.io", "istio.io"}
)

func init() {
	for _, s := range strings.Fields(`api apis v1 v2 v3 v1alpha1 v1alpha2 v1beta v1beta1 v2beta .well-known
		openid-configuration oauth-authorization-server oauth-protected-resource jwks jwks.json keys certs
		oauth oauth2 auth authorize token userinfo introspect revoke register login logout callback
		realms protocol openid-connect discovery mcp sse a2a agent chat completions messages responses
		embeddings models openai anthropic services hooks health healthz ready readyz metrics`) {
		plainSegments[s] = true
	}
	for _, s := range strings.Fields(`com net org io co edu gov mil biz info internal corp lan intra local test
		de uk fr es nl eu ca au jp cn ch se pl br`) {
		textTLDs[s] = true
	}
}

type anonymizer struct {
	pseudo map[string]string // "kind\x00value" -> pseudonym
	count  map[string]int
	hosts  map[string]bool // hostnames replaced so far, to find them again in free text
}

func sortedKeys(m map[string]any) []string { return slices.Sorted(maps.Keys(m)) }

func anonymize(objs []unstructured.Unstructured) *anonymizer {
	// A fixed order of objects and of keys makes the pseudonyms the same on
	// every run over the same objects.
	sort.SliceStable(objs, func(i, j int) bool {
		key := func(u *unstructured.Unstructured) string {
			return u.GetAPIVersion() + "/" + u.GetKind() + "/" + u.GetNamespace() + "/" + u.GetName()
		}
		return key(&objs[i]) < key(&objs[j])
	})
	a := &anonymizer{pseudo: map[string]string{}, count: map[string]int{}, hosts: map[string]bool{}}
	// The second pass replaces hostnames that the first one only learned about
	// later, or met in a place where nothing marks them as a host.
	for _, visit := range []func(v any, key, parent string) any{a.walk, a.sweep} {
		for i := range objs {
			o := objs[i].Object
			md, _ := o["metadata"].(map[string]any)
			for _, field := range []string{"labels", "annotations"} {
				// minimize has already dropped every key that is not on the allow list.
				// A label value goes through the same rules as the selector that
				// matches it, so the two stay equal.
				if m, ok := md[field].(map[string]any); ok {
					for _, k := range sortedKeys(m) {
						m[k] = visit(m[k], k, "")
					}
				}
			}
			for _, k := range sortedKeys(o) {
				if k != "apiVersion" && k != "kind" && k != "metadata" {
					o[k] = visit(o[k], k, "")
				}
			}
		}
	}
	return a
}

// walk anonymizes a value. key is the nearest map key above it and parent the
// one above that; items of a list share the key of the list.
func (a *anonymizer) walk(v any, key, parent string) any {
	switch t := v.(type) {
	case map[string]any:
		a.header(t)
		for _, k := range sortedKeys(t) {
			t[k] = a.walk(t[k], k, key)
		}
	case []any:
		for i, c := range t {
			t[i] = a.walk(c, key, parent)
		}
	case string:
		return a.scalar(key, parent, t)
	case int64:
		if idKeyRe.MatchString(key) {
			return a.name(key, strconv.FormatInt(t, 10))
		}
	case float64: // a snapshot read back from JSON
		if idKeyRe.MatchString(key) {
			return a.name(key, strconv.FormatFloat(t, 'f', -1, 64))
		}
	}
	return v
}

// header handles {name: Authorization, value: ...} and {name: Host, value: ...},
// where only the sibling tells what the value is.
func (a *anonymizer) header(m map[string]any) {
	name, _ := m["name"].(string)
	value, _ := m["value"].(string)
	switch {
	case name == "" || value == "":
	case secretHeaderRe.MatchString(name):
		m["value"] = redacted
	case hostHeaderRe.MatchString(name) && bareHostRe.MatchString(value):
		m["value"] = a.hostPort(value)
	}
}

// sweep is the second pass: hostnames only.
func (a *anonymizer) sweep(v any, key, _ string) any {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range sortedKeys(t) {
			t[k] = a.sweep(t[k], k, "")
		}
	case []any:
		for i, c := range t {
			t[i] = a.sweep(c, key, "")
		}
	case string:
		if !isIdentifier(key, t) {
			return a.hostsIn(t)
		}
	}
	return v
}

// isIdentifier reports whether s, under key, is a Kubernetes name or an enum.
func isIdentifier(key, s string) bool {
	return plainKeys[key] && !strings.ContainsAny(s, " @") && !strings.Contains(s, "://")
}

// scalar anonymizes one string value, using the keys above it as a hint.
func (a *anonymizer) scalar(key, parent, s string) string {
	switch {
	case s == "" || isIdentifier(key, s):
		return s
	case plainKeys[key]:
		return a.text(s)
	case secretKeyRe.MatchString(key) || strings.Contains(s, "-----BEGIN ") ||
		(key == "inline" && inlineParentRe.MatchString(parent)):
		return redacted
	case idKeyRe.MatchString(key):
		return a.name(key, s)
	case key == "service" && parent == "method": // gRPC: com.company.orders.v1.Orders
		return a.name("grpc", s)
	}
	if (hostKeyRe.MatchString(key) || hostKeyRe.MatchString(parent)) && bareHostRe.MatchString(s) {
		return a.hostPort(s)
	}
	return a.text(s)
}

// text anonymizes the identifying parts found inside a longer string.
func (a *anonymizer) text(s string) string {
	s = jwtRe.ReplaceAllString(s, redacted)
	s = credentialRe.ReplaceAllString(s, redacted)
	s = urlRe.ReplaceAllStringFunc(s, a.url)
	s = emailRe.ReplaceAllStringFunc(s, func(m string) string { return a.name("user", m) + "@example" })
	s = arnRe.ReplaceAllStringFunc(s, func(m string) string {
		p := arnRe.FindStringSubmatch(m)
		out := p[1] + a.name("account", p[2])
		if res := p[3]; res != "" { // ":role/name" keeps the type
			i := strings.IndexAny(res[1:], "/:") + 1
			out += res[:i+1] + a.name("res", res[i+1:])
		}
		return out
	})
	s = gcpProjectRe.ReplaceAllStringFunc(s, func(m string) string {
		return "projects/" + a.name("project", strings.TrimPrefix(m, "projects/"))
	})
	s = guidRe.ReplaceAllStringFunc(s, func(m string) string { return a.name("id", strings.ToLower(m)) })
	// In-cluster service names that are not part of a URL, e.g. "db.ns.svc:5432/name".
	s = svcInTextRe.ReplaceAllStringFunc(s, func(m string) string {
		if pseudoSvcRe.MatchString(m) {
			return m
		}
		return a.host(m)
	})
	s = ipRe.ReplaceAllStringFunc(s, a.ip)
	return a.hostsIn(a.ip6In(s))
}

// ip6In replaces IPv6 addresses. The pattern is loose (hex digits around two
// colons), so a match only counts when it stands alone and parses.
func (a *anonymizer) ip6In(s string) string {
	alnum := func(i int) bool {
		return i >= 0 && i < len(s) && (s[i] == '_' || s[i] >= '0' && s[i] <= '9' || s[i]|0x20 >= 'a' && s[i]|0x20 <= 'z')
	}
	var b strings.Builder
	last := 0
	for _, m := range ip6Re.FindAllStringIndex(s, -1) {
		if alnum(m[0]-1) || alnum(m[1]) {
			continue
		}
		b.WriteString(s[last:m[0]])
		b.WriteString(a.ip(s[m[0]:m[1]]))
		last = m[1]
	}
	return b.String() + s[last:]
}

// url anonymizes one URL: the host, the path segments that are not well-known
// words, and everything from the query on, which is dropped.
func (a *anonymizer) url(m string) string {
	p := urlRe.FindStringSubmatch(m)
	scheme, authority, rest := p[1], p[2], p[3]
	if i := strings.LastIndex(authority, "@"); i >= 0 { // credentials in the URL
		authority = authority[i+1:]
	}
	host := a.hostPort(authority)
	if host == authority && !strings.Contains(authority, ".") && authority != "localhost" {
		// A single label is a Service name behind http(s), and a bucket, a
		// vault or a registry behind anything else.
		switch strings.ToLower(strings.TrimSuffix(scheme, "://")) {
		case "http", "https", "ws", "wss", "grpc", "grpcs", "h2c", "tcp", "tls":
		default:
			host = a.name("h", authority)
		}
	}
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	segs := strings.Split(rest, "/")
	for i, seg := range segs {
		if seg != "" && !plainSegments[strings.ToLower(seg)] && !versionRe.MatchString(seg) {
			segs[i] = a.name("p", seg)
		}
	}
	return scheme + host + strings.Join(segs, "/")
}

// hostsIn replaces the hostnames found in free text: those that were already
// replaced somewhere else, and those under a common top-level domain.
func (a *anonymizer) hostsIn(s string) string {
	return dottedRe.ReplaceAllStringFunc(s, func(m string) string {
		h := strings.ToLower(m)
		if h != m || pseudoSvcRe.MatchString(h) { // camelCase is code, not a hostname
			return m
		}
		known := textTLDs[h[strings.LastIndex(h, ".")+1:]]
		for rest := h; !known && strings.Contains(rest, "."); { // h itself, or a parent domain of it
			known = a.hosts[rest]
			_, rest, _ = strings.Cut(rest, ".")
		}
		if !known {
			return m
		}
		return a.host(h)
	})
}

func (a *anonymizer) hostPort(s string) string {
	host, port := s, ""
	if i := strings.LastIndex(s, ":"); i >= 0 && !strings.Contains(s, "]") {
		if _, err := strconv.Atoi(s[i+1:]); err == nil {
			host, port = s[:i], s[i:]
		}
	}
	return a.host(host) + port
}

// host maps a hostname to a pseudonym of the same shape.
//
// The base domain (the last two labels, or everything from an embedded IPv4
// address on, as in app.10.0.0.1.nip.io) becomes one label, and every label in
// front of it gets a pseudonym of its own: api.eu.corp.com -> h1.h2.d1.example.
// A wildcard listener and the hosts under it therefore still share a suffix.
// In-cluster service names keep their form: svc-1.ns-1.svc.cluster.local.
func (a *anonymizer) host(h string) string {
	h = strings.ToLower(h)
	if net.ParseIP(strings.Trim(h, "[]")) != nil {
		return a.ip(strings.Trim(h, "[]"))
	}
	if h == "localhost" || !strings.Contains(h, ".") || strings.HasSuffix(h, ".example") {
		return h
	}
	for _, d := range apiDomains {
		if h == d || strings.HasSuffix(h, "."+d) {
			return h
		}
	}
	if m := clusterSvcRe.FindStringSubmatch(h); m != nil {
		return a.name("svc", m[1]) + "." + a.name("ns", m[2]) + ".svc" + m[3]
	}
	a.hosts[h] = true
	labels := strings.Split(h, ".")
	base := len(labels) - 2
	for i := 0; i+4 <= len(labels); i++ {
		if net.ParseIP(strings.Join(labels[i:i+4], ".")) != nil {
			base = i
			break
		}
	}
	out := make([]string, 0, base+2)
	for _, l := range labels[:base] {
		if l != "*" {
			l = a.name("h", l)
		}
		out = append(out, l)
	}
	out = append(out, a.name("d", strings.Join(labels[base:], ".")), "example")
	return strings.Join(out, ".")
}

func (a *anonymizer) ip(s string) string {
	ip := net.ParseIP(s)
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
		return s
	}
	return a.name("ip", ip.String())
}

// warning anonymizes a message from the API server or the client, which names
// the user that was refused and the address of the server.
func (a *anonymizer) warning(s string) string {
	return a.text(userRe.ReplaceAllString(s, `User "`+redacted+`"`))
}

var (
	userRe    = regexp.MustCompile(`User "[^"]*"`)
	versionRe = regexp.MustCompile(`^v\d+(\.\d+)*([a-z]+\d*)?$`) // a URL path segment: v1, v2.0, v1beta1
)

// name returns the pseudonym of a value: prefix-1, prefix-2, ... in order of
// first appearance.
func (a *anonymizer) name(prefix, value string) string {
	k := prefix + "\x00" + value
	if p, ok := a.pseudo[k]; ok {
		return p
	}
	a.count[prefix]++
	sep := "-"
	if prefix == "h" || prefix == "d" {
		sep = ""
	}
	p := fmt.Sprintf("%s%s%d", prefix, sep, a.count[prefix])
	a.pseudo[k] = p
	return p
}
