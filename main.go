// agwmap reads the gateway configuration of a Kubernetes cluster (Gateway API,
// agentgateway and kgateway, open source and enterprise) and renders it as one
// self-contained HTML file that shows how the objects relate to each other.
package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/tools/clientcmd"
)

//go:embed web/index.html
var page []byte

const (
	dataMarker   = "__GWMAP_DATA__"
	snapshotKind = "agwmap-snapshot"
)

// Snapshot is the captured objects, without anything derived from them. It is
// what the page exports and what -from renders again.
type Snapshot struct {
	Kind    string           `json:"kind"`
	Meta    Meta             `json:"meta"`
	Objects []map[string]any `json:"objects"`
}

type options struct {
	kubeconfig, kubeContext, namespace, from, out string
	anonymize                                     bool
}

// version is set at build time with -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	// Installed as kubectl-agwmap, it is the kubectl plugin "kubectl agwmap".
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	if strings.HasPrefix(name, "kubectl-") {
		name = "kubectl " + strings.TrimPrefix(name, "kubectl-")
	}

	var o options
	var allNamespaces, showVersion bool
	flag.StringVar(&o.kubeconfig, "kubeconfig", "", "path to a kubeconfig (default: $KUBECONFIG, ~/.kube/config, then in-cluster)")
	flag.StringVar(&o.kubeContext, "context", "", "kubeconfig context to use (default: current context)")
	flag.StringVar(&o.namespace, "n", "", "only read this namespace (default: all namespaces)")
	flag.StringVar(&o.namespace, "namespace", "", "same as -n")
	flag.BoolVar(&allNamespaces, "A", false, "read all namespaces (the default; accepted for kubectl parity)")
	flag.BoolVar(&allNamespaces, "all-namespaces", false, "same as -A")
	flag.StringVar(&o.from, "from", "", "render a snapshot file exported earlier instead of reading a cluster")
	flag.StringVar(&o.out, "o", "agwmap.html", "output file, or - for stdout; a .json name writes a snapshot instead of the page")
	flag.BoolVar(&o.anonymize, "anonymize", true, "replace hostnames, URLs, addresses, account ids and inline credentials with pseudonyms, and keep only gateway labels and annotations (object names and namespaces are kept); -anonymize=false keeps the real values")
	flag.BoolVar(&showVersion, "version", false, "print the version and exit")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "%s renders the gateway configuration of a Kubernetes cluster as one HTML page.\n\nUsage: %s [flags]\n\n", name, name)
		flag.PrintDefaults()
	}
	flag.Parse()
	if showVersion {
		if info, ok := debug.ReadBuildInfo(); ok && version == "dev" && info.Main.Version != "" {
			version = info.Main.Version // installed with go install
		}
		fmt.Println(version)
		return
	}
	if flag.NArg() > 0 {
		// The flag package stops at the first argument that is not a flag, so
		// everything after it would be ignored without a word.
		fmt.Fprintf(os.Stderr, "%s: unexpected argument %q (to render a snapshot, use -from %s)\n\n", name, flag.Arg(0), flag.Arg(0))
		flag.Usage()
		os.Exit(2)
	}
	if allNamespaces {
		o.namespace = ""
	}

	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "agwmap:", err)
		os.Exit(1)
	}
}

func run(o options) error {
	var snap *Snapshot
	var err error
	if o.from != "" {
		snap, err = readSnapshot(o.from)
	} else {
		snap, err = capture(o)
	}
	if err != nil {
		return err
	}
	objs := make([]unstructured.Unstructured, len(snap.Objects))
	for i, obj := range snap.Objects {
		objs[i] = unstructured.Unstructured{Object: obj}
	}
	// Also for a snapshot that was captured with the real values: what leaves
	// this program is anonymized unless -anonymize=false says otherwise.
	if o.anonymize && !snap.Meta.Anonymized {
		minimize(objs, true)
		a := anonymize(objs) // sorts objs
		for i := range objs {
			snap.Objects[i] = objs[i].Object
		}
		for i, w := range snap.Meta.Warnings {
			snap.Meta.Warnings[i] = a.warning(w)
		}
		snap.Meta.Context = "cluster" // context names often carry an account or a company name
		snap.Meta.Anonymized = true
	}

	var data []byte
	if strings.HasSuffix(o.out, ".json") {
		data, err = json.MarshalIndent(snap, "", " ")
	} else {
		g := buildGraph(objs)
		g.Meta = snap.Meta
		// json.Marshal escapes <, > and &, so the result is safe inside <script>.
		if data, err = json.Marshal(g); err == nil {
			data = bytes.Replace(page, []byte(dataMarker), data, 1)
		}
	}
	if err != nil {
		return err
	}

	if o.out == "-" {
		_, err = os.Stdout.Write(data)
		return err
	}
	if err := os.WriteFile(o.out, data, 0o644); err != nil {
		return err
	}
	mode := "anonymized"
	if !snap.Meta.Anonymized {
		mode = "NOT anonymized"
	}
	fmt.Fprintf(os.Stderr, "%d objects from %s (%s) -> %s\n", len(snap.Objects), snap.Meta.Context, mode, o.out)
	return nil
}

func readSnapshot(file string) (*Snapshot, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var s Snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if s.Kind != snapshotKind {
		return nil, fmt.Errorf("%s is not an agwmap snapshot", file)
	}
	s.Objects = slices.DeleteFunc(s.Objects, func(o map[string]any) bool { return o == nil })
	return &s, nil
}

func capture(o options) (*Snapshot, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = o.kubeconfig
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules,
		&clientcmd.ConfigOverrides{CurrentContext: o.kubeContext})
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kube config: %w", err)
	}
	cfg.QPS, cfg.Burst, cfg.UserAgent = 50, 100, "agwmap"

	name := o.kubeContext
	if name == "" {
		if raw, err := cc.RawConfig(); err == nil {
			name = raw.CurrentContext
		}
	}
	if name == "" {
		name = "in-cluster"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	objs, warnings, err := collect(ctx, cfg, o.namespace)
	if err != nil {
		return nil, err
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	minimize(objs, false)

	s := &Snapshot{Kind: snapshotKind, Meta: Meta{
		Context:     name,
		Namespace:   o.namespace,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Warnings:    warnings,
	}}
	for i := range objs {
		s.Objects = append(s.Objects, objs[i].Object)
	}
	return s, nil
}
