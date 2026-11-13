package lab

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"
)

// Manifest describes a lab site (conformance/lab/<site>/manifest.json):
// its hosts, the deploys that change what they serve, and what a
// monitoring run over the deploys must find. File paths are relative to
// the file system the lab is started with (the conformance directory),
// so sites reuse other suites' data (conformance/content/sites/spa).
type Manifest struct {
	Description string `json:"description"` // free text
	// Seed seeds the per-response volatile values (core.SplitMix64).
	Seed uint64 `json:"seed"`
	// Start is the lab clock at the first deploy; each later deploy is
	// DeployIntervalHours later.
	Start               time.Time `json:"start"`
	DeployIntervalHours int       `json:"deploy_interval_hours"`
	Hosts               []Host    `json:"hosts"`
	Deploys             []Deploy  `json:"deploys"`
	Expect              Expect    `json:"expect"`
}

// Host is one name the lab's DNS server answers.
type Host struct {
	Name string `json:"name"`
	// Scope is "in" (served on 127.0.0.1, the program's scope allows it)
	// or "out" (served on 127.0.0.2, never in scope).
	Scope string `json:"scope"`
}

// Deploy is one version of the site: the files of the previous deploy,
// with Files added or replaced and Remove taken away.
type Deploy struct {
	Name   string `json:"name"` // recorded on every Hit served while it is live
	Files  []File `json:"files"`
	Remove []Ref  `json:"remove,omitempty"`
}

// Ref names a served path: a listener (ListenerHTTPS by default), a Host
// header value and a path.
type Ref struct {
	Listener string `json:"listener,omitempty"`
	Host     string `json:"host"`
	Path     string `json:"path"`
}

// listener is the Ref's listener (default ListenerHTTPS).
func (r Ref) listener() string {
	if r.Listener == "" {
		return ListenerHTTPS
	}
	return r.Listener
}

// key is the canonical form of a Ref.
func (r Ref) key() string { return r.listener() + " " + r.Host + " " + r.Path }

// File is one response the lab serves.
type File struct {
	Ref
	// Prefix serves every path that starts with Path.
	Prefix bool `json:"prefix,omitempty"`
	// File is the body (a path in the lab's file system); Type its
	// Content-Type; Headers a file of "Name: value" lines.
	File    string `json:"file,omitempty"`
	Type    string `json:"type,omitempty"`
	Headers string `json:"headers,omitempty"`
	// Template expands placeholders in the body (headers always are).
	Template bool `json:"template,omitempty"`
	// Status defaults to 200, or 302 with Redirect (a Location template;
	// {{path}} is the request path).
	Status   int    `json:"status,omitempty"`
	Redirect string `json:"redirect,omitempty"`
}

// Expect is what monitoring the deploys must produce (V2_DESIGN §10).
type Expect struct {
	// Planted are the level-3 changes, exactly (secret keys carry a
	// tenant-keyed hash and are matched up to the rule id).
	Planted []Planted `json:"planted"`
	// Lineage are the renamed bundles and their predecessors.
	Lineage []Lineage `json:"lineage"`
	// Alerts are notifier rules and how often each must fire.
	Alerts []AlertRule `json:"alerts"`
	// NeverRequested are paths in scope that a polite monitor never
	// fetches (robots.txt disallows them).
	NeverRequested []Ref `json:"never_requested"`
}

// Planted is one level-3 change a deploy introduces.
type Planted struct {
	Deploy string `json:"deploy"`
	// URL is the snapshot's URL; Change is "<op> <kind>|<key>".
	URL    string `json:"url"`
	Change string `json:"change"`
	// Regex is a search_changes regex query that must find the change.
	Regex string `json:"regex"`
}

// Lineage is a renamed bundle (URL, first served in Deploy) linked to its
// predecessor's URL (Parent).
type Lineage struct {
	Deploy string `json:"deploy"`
	URL    string `json:"url"`
	Parent string `json:"parent"`
}

// AlertRule is a notifier rule (Name, rule Source text) and Count, the
// number of alerts it must fire over the whole run.
type AlertRule struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Count  int    `json:"count"`
}

// Listener names.
const (
	// ListenerHTTPS is the in-scope TLS listener (127.0.0.1).
	ListenerHTTPS = "https"
	// ListenerHTTP is the in-scope plain HTTP listener (127.0.0.1).
	ListenerHTTP = "http"
	// ListenerOut is the out-of-scope HTTP listener (127.0.0.2).
	ListenerOut = "out"
)

// LoadManifest reads and validates a manifest.
func LoadManifest(fsys fs.FS, name string) (*Manifest, error) {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("lab: %s: %w", name, err)
	}
	if len(m.Deploys) == 0 || m.DeployIntervalHours <= 0 || m.Start.IsZero() {
		return nil, fmt.Errorf("lab: %s: needs deploys, start and deploy_interval_hours", name)
	}
	hosts := map[string]string{}
	for _, h := range m.Hosts {
		if h.Scope != "in" && h.Scope != "out" {
			return nil, fmt.Errorf("lab: host %s: scope must be in or out", h.Name)
		}
		hosts[h.Name] = h.Scope
	}
	names := map[string]bool{}
	for _, d := range m.Deploys {
		if names[d.Name] || d.Name == "" {
			return nil, fmt.Errorf("lab: deploy names must be unique and non-empty (%q)", d.Name)
		}
		names[d.Name] = true
		for _, f := range d.Files {
			scope, ok := hosts[f.Host]
			switch {
			case !ok:
				return nil, fmt.Errorf("lab: deploy %s: unknown host %s", d.Name, f.Host)
			case (f.Listener == ListenerOut) != (scope == "out"):
				return nil, fmt.Errorf("lab: deploy %s: %s is served on the wrong listener", d.Name, f.Host)
			case !strings.HasPrefix(f.Path, "/"):
				return nil, fmt.Errorf("lab: deploy %s: path %q", d.Name, f.Path)
			case f.File == "" && f.Redirect == "":
				return nil, fmt.Errorf("lab: deploy %s: %s needs a file or a redirect", d.Name, f.key())
			}
			for _, p := range []string{f.File, f.Headers} {
				if p != "" && !fs.ValidPath(path.Clean(p)) {
					return nil, fmt.Errorf("lab: deploy %s: bad file path %q", d.Name, p)
				}
			}
		}
	}
	for _, p := range m.Expect.Planted {
		if !names[p.Deploy] {
			return nil, fmt.Errorf("lab: planted change in unknown deploy %s", p.Deploy)
		}
	}
	return &m, nil
}
