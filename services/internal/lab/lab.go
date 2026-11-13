// Package lab is the in-process target environment of the V2 end-to-end
// acceptance test (spec 38; V2_DESIGN §10), built to also back later evaluation
// harnesses: TLS and plain HTTP servers for in-scope hosts on 127.0.0.1,
// an HTTP server for out-of-scope hosts on 127.0.0.2, and a fake
// authoritative DNS server for the lab zones. It serves a versioned site
// (Manifest) whose deploys change what the hosts serve, fills volatile
// values (nonces, timestamps, request ids) per response, and logs every
// request and query so a test can prove which hosts were contacted.
//
// Everything is deterministic except the listen ports: certificates come
// from fixed Ed25519 seeds, volatile values from core.SplitMix64 with the
// manifest's seed, and times from the lab Clock.
package lab

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/dnsmsg"
	"github.com/genuinebnt/flowforge/scope"
)

// Addresses of the two lab networks: in-scope hosts resolve to InAddr,
// out-of-scope hosts to OutAddr (both loopback on Linux, so the egress
// proxy needs an explicit cidr rule for InAddr and none ever names
// OutAddr).
var (
	InAddr  = netip.MustParseAddr("127.0.0.1")
	OutAddr = netip.MustParseAddr("127.0.0.2")
)

// Hit is one request a lab server received.
type Hit struct {
	Listener string // ListenerHTTPS, ListenerHTTP or ListenerOut
	Host     string // Host header without port, lower case
	SNI      string // TLS server name (https only)
	Method   string
	Path     string // path and query
	Deploy   string // the deploy live when the request arrived ("" before the first)
}

// Query is one DNS question the lab's server received.
type Query struct {
	Name string // canonical, without the trailing dot
	Type dnsmsg.Type
}

// Lab is a running lab.
type Lab struct {
	m    *Manifest
	fsys fs.FS
	// Clock is the lab's time: Deploy sets it, and volatile time values
	// and the Date header come from it (certificates have fixed dates).
	Clock *Clock

	ca      *x509.Certificate
	pool    *x509.CertPool
	servers []*http.Server
	dnsUDP  net.PacketConn
	dnsTCP  net.Listener
	ports   map[string]int
	dnsAddr string
	scopes  map[string]string // host -> in|out

	mu      sync.Mutex
	deploy  int
	files   map[string]File // Ref.key -> file
	prefix  []File
	hits    []Hit
	queries []Query
	rng     *core.SplitMix64
	served  int64
	wg      sync.WaitGroup
}

// Start loads the manifest name from fsys and starts every server. Call
// Deploy before the first request.
func Start(fsys fs.FS, name string) (*Lab, error) {
	m, err := LoadManifest(fsys, name)
	if err != nil {
		return nil, err
	}
	l := &Lab{m: m, fsys: fsys, Clock: NewClock(m.Start), ports: map[string]int{}, scopes: map[string]string{}, deploy: -1,
		rng: core.NewSplitMix64(m.Seed)}
	for _, h := range m.Hosts {
		l.scopes[h.Name] = h.Scope
	}
	if err := l.start(); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

func (l *Lab) start() error {
	leaf, err := l.certificates()
	if err != nil {
		return err
	}
	listen := func(addr netip.Addr) (net.Listener, error) {
		return net.Listen("tcp", netip.AddrPortFrom(addr, 0).String())
	}
	type srv struct {
		name string
		addr netip.Addr
		tls  bool
	}
	for _, s := range []srv{{ListenerHTTPS, InAddr, true}, {ListenerHTTP, InAddr, false}, {ListenerOut, OutAddr, false}} {
		ln, err := listen(s.addr)
		if err != nil {
			return fmt.Errorf("lab: %s listener on %s: %w", s.name, s.addr, err)
		}
		l.ports[s.name] = ln.Addr().(*net.TCPAddr).Port
		if s.tls {
			ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12})
		}
		hs := &http.Server{Handler: l.handler(s.name), ReadHeaderTimeout: 10 * time.Second}
		l.servers = append(l.servers, hs)
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			_ = hs.Serve(ln)
		}()
	}
	return l.startDNS()
}

// Close stops every server.
func (l *Lab) Close() {
	for _, s := range l.servers {
		_ = s.Close()
	}
	if l.dnsUDP != nil {
		_ = l.dnsUDP.Close()
	}
	if l.dnsTCP != nil {
		_ = l.dnsTCP.Close()
	}
	l.wg.Wait()
}

// Manifest returns the loaded manifest.
func (l *Lab) Manifest() *Manifest { return l.m }

// Deploys returns the number of deploys.
func (l *Lab) Deploys() int { return len(l.m.Deploys) }

// DeployName returns the name of deploy i.
func (l *Lab) DeployName(i int) string { return l.m.Deploys[i].Name }

// Port returns the port of a listener (ListenerHTTPS, ListenerHTTP,
// ListenerOut).
func (l *Lab) Port(listener string) int { return l.ports[listener] }

// DNSAddr is the DNS server's address (UDP and TCP).
func (l *Lab) DNSAddr() string { return l.dnsAddr }

// RootCAs trusts the lab's certificate authority.
func (l *Lab) RootCAs() *x509.CertPool { return l.pool }

// ClientTLS is a client TLS configuration that trusts only the lab CA.
func (l *Lab) ClientTLS() *tls.Config {
	return &tls.Config{RootCAs: l.pool, MinVersion: tls.VersionTLS12}
}

// Hosts returns the host names of a scope ("in" or "out"), sorted.
func (l *Lab) Hosts(scopeName string) []string {
	var out []string
	for h, s := range l.scopes {
		if s == scopeName {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}

// Deploy installs deploy i (0-based) and sets the lab clock to its time.
// Deploys are cumulative: deploy i serves deploy i-1's files with its own
// added, replaced or removed.
func (l *Lab) Deploy(i int) error {
	if i < 0 || i >= len(l.m.Deploys) {
		return fmt.Errorf("lab: no deploy %d", i)
	}
	files := map[string]File{}
	for j := 0; j <= i; j++ {
		d := l.m.Deploys[j]
		for _, r := range d.Remove {
			delete(files, r.key())
		}
		for _, f := range d.Files {
			files[f.key()] = f
		}
	}
	var prefix []File
	for _, f := range files {
		if f.Prefix {
			prefix = append(prefix, f)
		}
	}
	// Longest prefix first, so a specific route wins over a catch-all.
	sort.Slice(prefix, func(a, b int) bool {
		if len(prefix[a].Path) != len(prefix[b].Path) {
			return len(prefix[a].Path) > len(prefix[b].Path)
		}
		return prefix[a].key() < prefix[b].key()
	})
	l.mu.Lock()
	l.deploy, l.files, l.prefix = i, files, prefix
	l.mu.Unlock()
	l.Clock.Set(l.m.Start.Add(time.Duration(i*l.m.DeployIntervalHours) * time.Hour))
	return nil
}

// Hits returns every request received so far.
func (l *Lab) Hits() []Hit {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Hit(nil), l.hits...)
}

// Queries returns every DNS question received so far.
func (l *Lab) Queries() []Query {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Query(nil), l.queries...)
}

// Expand replaces the port placeholders {{https}}, {{http}} and
// {{out_http}} in s (manifest expectations use them).
func (l *Lab) Expand(s string) string {
	return strings.NewReplacer("{{https}}", strconv.Itoa(l.ports[ListenerHTTPS]), "{{http}}", strconv.Itoa(l.ports[ListenerHTTP]),
		"{{out_http}}", strconv.Itoa(l.ports[ListenerOut])).Replace(s)
}

// URL returns the URL of path on host through a listener.
func (l *Lab) URL(listener, host, p string) string {
	scheme := "http"
	if listener == ListenerHTTPS {
		scheme = "https"
	}
	return scheme + "://" + host + ":" + strconv.Itoa(l.ports[listener]) + p
}

// volatile returns the per-response placeholder values: a nonce, the
// lab time in Unix milliseconds and seconds, as RFC 3339, and a request
// id. Each response gets the next values of the seeded generator and one
// more millisecond, so no two responses are byte-identical.
func (l *Lab) volatile() *strings.Replacer {
	l.mu.Lock()
	a, b := l.rng.Uint64(), l.rng.Uint64()
	l.served++
	n := l.served
	l.mu.Unlock()
	at := l.Clock.Now().Add(time.Duration(n) * time.Millisecond)
	nonce := strconv.FormatUint(a, 36) + strconv.FormatUint(b, 36)
	if len(nonce) > 16 {
		nonce = nonce[:16]
	}
	rid := fmt.Sprintf("%08x-%04x-4%03x-8%03x-%012x", uint32(a), uint16(a>>32), uint16(a>>48)&0xfff, uint16(b)&0xfff, b>>16)
	return strings.NewReplacer("{{nonce}}", nonce, "{{ts}}", strconv.FormatInt(at.UnixMilli(), 10),
		"{{ts_s}}", strconv.FormatInt(at.Unix(), 10), "{{iso}}", at.UTC().Format(time.RFC3339), "{{request_id}}", rid,
		"{{https}}", strconv.Itoa(l.ports[ListenerHTTPS]), "{{http}}", strconv.Itoa(l.ports[ListenerHTTP]),
		"{{out_http}}", strconv.Itoa(l.ports[ListenerOut]))
}

func (l *Lab) handler(listener string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(r.Host)
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		hit := Hit{Listener: listener, Host: host, Method: r.Method, Path: r.URL.RequestURI()}
		if r.TLS != nil {
			hit.SNI = r.TLS.ServerName
		}
		l.mu.Lock()
		if l.deploy >= 0 {
			hit.Deploy = l.m.Deploys[l.deploy].Name
		}
		l.hits = append(l.hits, hit)
		f, ok := l.files[Ref{Listener: listener, Host: host, Path: r.URL.Path}.key()]
		if !ok {
			for _, p := range l.prefix {
				if p.listener() == listener && p.Host == host && strings.HasPrefix(r.URL.Path, p.Path) {
					f, ok = p, true
					break
				}
			}
		}
		l.mu.Unlock()
		rep := l.volatile()
		w.Header().Set("Date", l.Clock.Now().UTC().Format(http.TimeFormat))
		w.Header().Set("X-Request-Id", rep.Replace("{{request_id}}"))
		if !ok {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("<!DOCTYPE html><html><head><title>Not found</title></head><body><p>Not found</p></body></html>\n"))
			return
		}
		if err := l.serve(w, r, f, rep); err != nil {
			http.Error(w, "lab: "+err.Error(), http.StatusInternalServerError)
		}
	})
}

func (l *Lab) serve(w http.ResponseWriter, r *http.Request, f File, rep *strings.Replacer) error {
	if f.Headers != "" {
		b, err := fs.ReadFile(l.fsys, path.Clean(f.Headers))
		if err != nil {
			return err
		}
		for _, line := range strings.Split(rep.Replace(string(b)), "\n") {
			k, v, ok := strings.Cut(line, ":")
			if ok && strings.TrimSpace(k) != "" {
				w.Header().Add(strings.TrimSpace(k), strings.TrimSpace(v))
			}
		}
	}
	status := f.Status
	if f.Redirect != "" {
		if status == 0 {
			status = http.StatusFound
		}
		w.Header().Set("Location", strings.ReplaceAll(rep.Replace(f.Redirect), "{{path}}", r.URL.RequestURI()))
	}
	if status == 0 {
		status = http.StatusOK
	}
	var body []byte
	if f.File != "" {
		b, err := fs.ReadFile(l.fsys, path.Clean(f.File))
		if err != nil {
			return err
		}
		body = b
		if f.Template {
			body = []byte(rep.Replace(string(b)))
		}
	}
	if f.Type != "" {
		w.Header().Set("Content-Type", f.Type)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
	return nil
}

// certificates builds the lab CA and the leaf certificate of every
// in-scope host, both Ed25519 from fixed seeds (deterministic; Ed25519
// signatures need no randomness).
func (l *Lab) certificates() (tls.Certificate, error) {
	seed := func(tag byte) ed25519.PrivateKey {
		s := make([]byte, ed25519.SeedSize)
		for i := range s {
			s[i] = tag + byte(i)
		}
		return ed25519.NewKeyFromSeed(s)
	}
	caKey, leafKey := seed(0xca), seed(0x1e)
	notBefore := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	notAfter := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Flowforge lab CA"},
		NotBefore: notBefore, NotAfter: notAfter, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(nil, caTmpl, caTmpl, caKey.Public(), caKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("lab: CA: %w", err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return tls.Certificate{}, err
	}
	names := l.Hosts("in")
	if len(names) == 0 {
		return tls.Certificate{}, errors.New("lab: no in-scope hosts")
	}
	leafTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: notBefore, NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(nil, leafTmpl, ca, leafKey.Public(), caKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("lab: leaf: %w", err)
	}
	l.ca = ca
	l.pool = x509.NewCertPool()
	l.pool.AddCert(ca)
	return tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}, nil
}

// Resolver resolves names through the lab's DNS server (A records), the
// scope service's and the egress proxy's view of the lab.
func (l *Lab) Resolver() scope.Resolver { return resolver{addr: l.dnsAddr} }

type resolver struct{ addr string }

// LookupAddrs implements scope.Resolver.
func (r resolver) LookupAddrs(ctx context.Context, host string) ([]netip.Addr, error) {
	ans, rcode, err := exchange(ctx, r.addr, host, dnsmsg.TypeA)
	if err != nil {
		return nil, err
	}
	if rcode == dnsmsg.RCodeNXDomain || len(ans) == 0 {
		return nil, fmt.Errorf("%w: %s", scope.ErrNoSuchHost, host)
	}
	var out []netip.Addr
	for _, rr := range ans {
		if rr.Data.Kind == dnsmsg.KindA {
			out = append(out, rr.Data.IP)
		}
	}
	return out, nil
}
