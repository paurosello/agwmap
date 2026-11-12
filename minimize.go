package main

import (
	"regexp"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// What a capture keeps of each object:
//
//	apiVersion, kind
//	metadata.name, metadata.namespace
//	metadata.labels and metadata.annotations  (see the allow list below)
//	spec and any other top-level field of the kind
//	status, without timestamps and generation counters
//
// Everything else in metadata (uid, resourceVersion, generation, timestamps,
// owner references, finalizers, managed fields) is dropped.

// Labels and annotations are kept only when their key belongs to one of these
// domains, which are the ones the gateway products read.
var allowedKeyRe = regexp.MustCompile(
	`^([a-z0-9.-]+\.)?(gateway\.networking\.k8s\.io|gateway\.networking\.x-k8s\.io|kgateway\.dev|agentgateway\.dev|solo\.io|istio\.io)/`)

// Repeats the whole object, so it is never kept.
const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"

// minimize strips every object down to the fields above. With allowList set,
// labels and annotations are filtered by key; a label that a policy selects its
// targets by (targetSelectors) is kept too, because the selection cannot be
// shown without it.
func minimize(objs []unstructured.Unstructured, allowList bool) {
	selected := map[string]bool{}
	for i := range objs {
		sels, _, _ := unstructured.NestedSlice(objs[i].Object, "spec", "targetSelectors")
		for _, sel := range sels {
			m, _ := sel.(map[string]any)
			ml, _ := m["matchLabels"].(map[string]any)
			for k := range ml {
				selected[k] = true
			}
		}
	}
	keep := func(m map[string]string, isLabel bool) map[string]any {
		out := map[string]any{}
		for k, v := range m {
			if k == lastApplied {
				continue
			}
			if !allowList || allowedKeyRe.MatchString(k) || (isLabel && selected[k]) {
				out[k] = v
			}
		}
		return out
	}
	for i := range objs {
		u := &objs[i]
		md := map[string]any{"name": u.GetName()}
		if ns := u.GetNamespace(); ns != "" {
			md["namespace"] = ns
		}
		if l := keep(u.GetLabels(), true); len(l) > 0 {
			md["labels"] = l
		}
		if a := keep(u.GetAnnotations(), false); len(a) > 0 {
			md["annotations"] = a
		}
		u.Object["metadata"] = md
		dropKeys(u.Object["status"], "lastTransitionTime", "observedGeneration")
	}
}

func dropKeys(v any, keys ...string) {
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			dropKeys(e, keys...)
		}
	case map[string]any:
		for _, k := range keys {
			delete(t, k)
		}
		for _, c := range t {
			dropKeys(c, keys...)
		}
	}
}
