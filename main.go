// agwmap reads the gateway configuration of a Kubernetes cluster (Gateway API,
// agentgateway and kgateway, open source and enterprise) and renders it as one
// self-contained HTML file that shows how the objects relate to each other.
package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

//go:embed web/index.html
var page []byte

const (
	dataMarker   = "__GWMAP_DATA__"
	snapshotKind = "agwmap-snapshot"
)

// Snapshot is the captured objects, without anything derived from them. It is
// what the page exports and what -from renders again.
//
// As a file it is a multi-document YAML manifest: one document of kind
// agwmap-snapshot that holds Meta, then one document per object.
type Snapshot struct {
	Meta    Meta
	Objects []map[string]any
}

// snapshotHead is the first document of a snapshot file.
type snapshotHead struct {
	Kind string `json:"kind"`
	Meta Meta   `json:"meta"`
}

type options struct {
	kubeconfig, kubeContext, namespace, from, out string
	anonymize, open                               bool
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
	flag.StringVar(&o.from, "from", "", "render a YAML file, local or at an http(s) URL, instead of reading a cluster: a snapshot exported earlier, or any manifest of gateway objects")
	flag.StringVar(&o.out, "o", "", "output file, or - for stdout; a .yaml name writes a snapshot instead of the page (default: a new file in the temp folder)")
	flag.BoolVar(&o.open, "open", true, "open the page in the default browser when stderr is a terminal; -open=false only writes it")
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
		fmt.Fprintf(os.Stderr, "%s: unexpected argument %q (to render a file, use -from %s)\n\n", name, flag.Arg(0), flag.Arg(0))
		flag.Usage()
		os.Exit(2)
	}
	if allNamespaces {
		o.namespace = ""
	}

	// Look for a newer release while the map is made, and open the page in a
	// browser. Only when someone is there: not in a pipeline or a CI job.
	interactive := false
	if st, err := os.Stderr.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
		interactive = true
	}
	o.open = o.open && interactive
	notice := make(chan string, 1)
	if interactive {
		go func() { notice <- updateNotice(version) }()
	} else {
		notice <- ""
	}

	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "agwmap:", err)
		os.Exit(1)
	}
	if n := <-notice; n != "" {
		fmt.Fprintln(os.Stderr, n)
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
	// Also for a manifest, or a snapshot that was captured with the real values:
	// what leaves this program is anonymized unless -anonymize=false says otherwise.
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
	isSnapshot := strings.HasSuffix(o.out, ".yaml") || strings.HasSuffix(o.out, ".yml")
	if isSnapshot {
		data, err = snap.yaml()
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
	if o.out == "" {
		// A new file each run, readable only by its owner: it holds cluster data.
		f, err := os.CreateTemp("", "agwmap-*.html")
		if err != nil {
			return err
		}
		o.out = f.Name()
		if _, err := f.Write(data); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	} else if err := os.WriteFile(o.out, data, 0o644); err != nil {
		return err
	}
	mode := "anonymized"
	if !snap.Meta.Anonymized {
		mode = "NOT anonymized"
	}
	fmt.Fprintf(os.Stderr, "%d objects from %s (%s) -> %s\n", len(snap.Objects), snap.Meta.Context, mode, o.out)
	if o.open && !isSnapshot {
		if err := openBrowser(o.out); err != nil {
			fmt.Fprintf(os.Stderr, "agwmap: could not open a browser (%v); open %s yourself\n", err, o.out)
		}
	}
	return nil
}

// openBrowser shows a local file in the default browser and does not wait for it.
func openBrowser(file string) error {
	abs, err := filepath.Abs(file)
	if err != nil {
		return err
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", abs)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", abs)
	default:
		cmd = exec.Command("xdg-open", abs)
	}
	return cmd.Start()
}

// yaml is the snapshot as a file.
func (s *Snapshot) yaml() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("# agwmap snapshot. Render it with: kubectl agwmap -from FILE\n")
	head, err := yaml.Marshal(snapshotHead{snapshotKind, s.Meta})
	if err != nil {
		return nil, err
	}
	b.Write(head)
	for _, obj := range s.Objects {
		raw, err := yaml.Marshal(obj)
		if err != nil {
			return nil, err
		}
		b.WriteString("---\n")
		b.Write(raw)
	}
	return b.Bytes(), nil
}

// readSnapshot reads a multi-document YAML file, local or at an http(s) URL: a
// snapshot that agwmap wrote, or any manifest, such as what kubectl apply -f
// takes or the List that kubectl get -o yaml prints.
func readSnapshot(src string) (*Snapshot, error) {
	var in io.Reader
	shown := src // what messages and the page call the source
	if isURL(src) {
		body, clean, err := fetch(src)
		if err != nil {
			return nil, err
		}
		in, shown = bytes.NewReader(body), clean
	} else {
		f, err := os.Open(src)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		in = bufio.NewReader(f)
	}

	// What a manifest does not say about itself; a snapshot's first document does.
	s := &Snapshot{Meta: Meta{Context: filepath.Base(shown), GeneratedAt: time.Now().UTC().Format(time.RFC3339)}}
	objs, head, err := readManifest(in)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", shown, err)
	}
	if head != nil {
		s.Meta = *head
	} else if len(objs) == 0 {
		return nil, fmt.Errorf("%s holds no object of the gateway API groups", shown)
	}
	// A manifest taken from a cluster carries what a capture drops.
	minimize(objs, false)
	for i := range objs {
		s.Objects = append(s.Objects, objs[i].Object)
	}
	return s, nil
}

// readManifest returns the objects of the gateway API groups in a
// multi-document YAML stream, and the Meta of its snapshot document if it has
// one. Like a capture, it keeps nothing else, so a Secret or a ConfigMap in the
// stream is never read further.
func readManifest(in io.Reader) (objs []unstructured.Unstructured, head *Meta, err error) {
	docs := utilyaml.NewYAMLReader(bufio.NewReader(in))
	for {
		raw, err := docs.Read()
		if err == io.EOF {
			return objs, head, nil
		}
		if err != nil {
			return nil, nil, err
		}
		var doc map[string]any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, nil, err
		}
		items := []any{doc}
		switch doc["kind"] {
		case snapshotKind:
			var h snapshotHead
			if err := yaml.Unmarshal(raw, &h); err != nil {
				return nil, nil, err
			}
			head = &h.Meta
			continue
		case "List":
			items, _ = doc["items"].([]any)
		}
		for _, item := range items {
			obj, _ := item.(map[string]any)
			apiVersion, _ := obj["apiVersion"].(string)
			group, _, grouped := strings.Cut(apiVersion, "/")
			if kind, _ := obj["kind"].(string); kind != "" && grouped && wantGroup(group) {
				objs = append(objs, unstructured.Unstructured{Object: obj})
			}
		}
	}
}

func isURL(src string) bool {
	return strings.HasPrefix(src, "https://") || strings.HasPrefix(src, "http://")
}

// maxFetch is the largest file -from downloads.
const maxFetch = 32 << 20

// fetch downloads the file at a URL. It also returns the URL without its query
// and credentials, which is the only form that is printed or kept: the link to
// a file in a private repository can carry a token.
func fetch(src string) (body []byte, shown string, err error) {
	u, err := url.Parse(src)
	if err != nil || u.Host == "" {
		return nil, "", fmt.Errorf("-from: not a valid URL")
	}
	shown = (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
	req, err := http.NewRequest(http.MethodGet, src, nil)
	if err != nil {
		return nil, "", fmt.Errorf("-from: not a valid URL")
	}
	req.Header.Set("User-Agent", "agwmap")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // without the URL as it was given
		}
		return nil, "", fmt.Errorf("%s: %w", shown, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("%s: %s", shown, resp.Status)
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		return nil, "", fmt.Errorf("%s is a web page, not a YAML file: use the link to the raw file", shown)
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, maxFetch+1))
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", shown, err)
	}
	if len(body) > maxFetch {
		return nil, "", fmt.Errorf("%s is larger than %d MiB", shown, maxFetch>>20)
	}
	return body, shown, nil
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

	s := &Snapshot{Meta: Meta{
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
