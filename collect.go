package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// API groups that hold gateway configuration. Everything served under them is
// read, so new kinds show up without a code change. Secrets, ConfigMaps and
// Services are never read: they only appear as names that something refers to.
var (
	exactGroups = []string{
		"gateway.networking.k8s.io",
		"gateway.networking.x-k8s.io",
		"inference.networking.k8s.io", // InferencePool, a route backend
		"inference.networking.x-k8s.io",
		"agentgateway.dev",
		"gateway.kgateway.dev",
		"enterprise.solo.io",
		"extauth.solo.io",
		"ratelimit.solo.io",
		"waf.solo.io",
	}
	groupSuffixes = []string{".agentgateway.dev", ".kgateway.dev", "agentgateway.solo.io", "kgateway.solo.io"}
)

func wantGroup(g string) bool {
	if slices.Contains(exactGroups, g) {
		return true
	}
	for _, s := range groupSuffixes {
		if strings.HasSuffix(g, s) {
			return true
		}
	}
	return false
}

// collect lists every object of the gateway API groups. Kinds that cannot be
// listed (RBAC, broken aggregated API) become warnings, not failures.
func collect(ctx context.Context, cfg *rest.Config, namespace string) ([]unstructured.Unstructured, []string, error) {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	lists, warnings, err := discover(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}

	var objs []unstructured.Unstructured
	for _, l := range lists {
		gv, err := schema.ParseGroupVersion(l.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range l.APIResources {
			if strings.Contains(r.Name, "/") || !slices.Contains(r.Verbs, "list") {
				continue
			}
			ns := ""
			if r.Namespaced {
				ns = namespace
			}
			ri := dyn.Resource(gv.WithResource(r.Name)).Namespace(ns)
			opts := metav1.ListOptions{Limit: 500}
			for {
				page, err := ri.List(ctx, opts)
				if err != nil {
					warnings = append(warnings, fmt.Sprintf("could not list %s.%s: %v", r.Name, gv.Group, err))
					break
				}
				objs = append(objs, page.Items...)
				if page.GetContinue() == "" {
					break
				}
				opts.Continue = page.GetContinue()
			}
		}
	}
	return objs, warnings, nil
}

// discover returns the resources of the gateway API groups, each resource once.
//
// It asks the server directly (/apis, then every version of a wanted group)
// instead of using client-go's discovery package, which links the typed API of
// every built-in Kubernetes group into the binary and more than doubles it.
func discover(ctx context.Context, cfg *rest.Config) ([]*metav1.APIResourceList, []string, error) {
	c := rest.CopyConfig(cfg)
	c.NegotiatedSerializer = serializer.NewCodecFactory(runtime.NewScheme()).WithoutConversion()
	rc, err := rest.UnversionedRESTClientFor(c)
	if err != nil {
		return nil, nil, err
	}
	raw, err := rc.Get().AbsPath("/apis").DoRaw(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("discover API groups: %w", err)
	}
	var groups metav1.APIGroupList
	if err := json.Unmarshal(raw, &groups); err != nil {
		return nil, nil, fmt.Errorf("discover API groups: %w", err)
	}

	var lists []*metav1.APIResourceList
	var warnings []string
	for _, g := range groups.Groups {
		if !wantGroup(g.Name) {
			continue
		}
		// The preferred version first, then the others: a resource that only an
		// older version serves (ReferenceGrant, in v1beta1) must not be missed.
		versions := []string{g.PreferredVersion.GroupVersion}
		for _, v := range g.Versions {
			if v.GroupVersion != g.PreferredVersion.GroupVersion {
				versions = append(versions, v.GroupVersion)
			}
		}
		seen := map[string]bool{}
		for _, gv := range versions {
			if gv == "" {
				continue
			}
			raw, err := rc.Get().AbsPath("/apis/" + gv).DoRaw(ctx)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("could not discover %s: %v", gv, err))
				continue
			}
			l := &metav1.APIResourceList{}
			if err := json.Unmarshal(raw, l); err != nil {
				warnings = append(warnings, fmt.Sprintf("could not discover %s: %v", gv, err))
				continue
			}
			l.GroupVersion = gv
			l.APIResources = slices.DeleteFunc(l.APIResources, func(r metav1.APIResource) bool {
				dup := seen[r.Name]
				seen[r.Name] = true
				return dup
			})
			lists = append(lists, l)
		}
	}
	return lists, warnings, nil
}
