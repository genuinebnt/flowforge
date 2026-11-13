package toolrunner

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/scope"
)

// PortScan is the active portscan tool (spec 6.4 "Port scanning", V2_DESIGN
// §8): a TCP connect through the egress proxy (CONNECT, or SOCKS5) per
// port, paced per target, emitting a Service and a port edge (PortRelation)
// for each open port. The proxy enforces the program rate per target
// whatever the tool's parallelism; the tool paces itself to the spec rate
// and retries rate-limit refusals after the proxy's retry hint.
type PortScan struct {
	// Pace is the minimum interval between connection attempts (default
	// 1/RateRPS of the spec, 100 ms).
	Pace time.Duration
	// Parallel is the number of concurrent attempts (default 4).
	Parallel int
	// DialTimeout bounds one attempt (default 5 s).
	DialTimeout time.Duration
	// Retries is how often a rate-limited attempt is retried (default 3;
	// negative: never).
	Retries int
	// name and version distinguish the tcpconnect alias.
	name, version, desc string
}

// TCPConnect is the tcpconnect alias of portscan (kept for existing
// workflows; identical behaviour).
type TCPConnect struct{ PortScan }

// Spec implements Adapter.
func (t TCPConnect) Spec() Spec {
	p := t.PortScan
	p.name, p.version, p.desc = "tcpconnect", "1.1.0", "TCP connect check through the egress proxy (alias of portscan)"
	return p.Spec()
}

// Run implements Adapter.
func (t TCPConnect) Run(ctx context.Context, input json.RawMessage, tc *ToolCtx, out Emitter) (json.RawMessage, error) {
	p := t.PortScan
	p.name = "tcpconnect"
	return p.Run(ctx, input, tc, out)
}

// Spec implements Adapter.
func (p PortScan) Spec() Spec {
	s := Spec{Name: "portscan", Version: "2.0.0", Description: "TCP connect port scan through the egress proxy, paced per target",
		InputType: "(InScope<Host>, List<Int>)", OutputType: "List<Int>", Observes: []string{oam.TypeService, oam.TypeFQDN, oam.TypeIPAddress},
		Class: core.ClassActive, RateRPS: 10, RateBurst: 20, Concurrency: 4, Timeout: 10 * time.Minute, Idempotent: true,
		Idempotency: engine.IdempotencyIdempotent, Retry: RetryTransient}
	if p.name != "" {
		s.Name, s.Version, s.Description = p.name, p.version, p.desc
	}
	return s
}

// MaxPorts bounds one portscan call.
const MaxPorts = 1000

// Run implements Adapter.
func (p PortScan) Run(ctx context.Context, input json.RawMessage, tc *ToolCtx, out Emitter) (json.RawMessage, error) {
	args, err := decodeArgs(input, 2)
	if err != nil {
		return nil, err
	}
	host, err := decodeHost(args[0])
	if err != nil {
		return nil, err
	}
	var ports []int
	if err := json.Unmarshal(args[1], &ports); err != nil || len(ports) > MaxPorts {
		return nil, Errorf("invalid_input", false, "ports must be a list of at most 1000 integers")
	}
	ports = slices.DeleteFunc(slices.Clone(ports), func(x int) bool { return x < 1 || x > 65535 })
	slices.Sort(ports)
	ports = slices.Compact(ports)
	nw := tc.Network()
	if nw == nil {
		return nil, ErrNoEgress
	}
	d, ok, err := tc.Allowed(ctx, out, host, scope.TargetHost, core.ClassActive)
	if err != nil {
		return nil, err
	}
	open := []int{}
	if !ok {
		return json.Marshal(open)
	}
	asset, key, err := hostAsset(host)
	if err != nil {
		return nil, Errorf("invalid_input", false, err.Error())
	}
	pace := p.Pace
	if pace == 0 {
		pace = time.Duration(float64(time.Second) / p.Spec().RateRPS)
	}
	par := p.Parallel
	if par <= 0 {
		par = 4
	}
	dialTO := p.DialTimeout
	if dialTO <= 0 {
		dialTO = 5 * time.Second
	}
	retries := p.Retries
	if retries == 0 {
		retries = 3
	}
	var (
		mu       sync.Mutex
		next     time.Time
		openSet  = map[int]bool{}
		done     int64
		jobs     = make(chan int)
		wg       sync.WaitGroup
		canceled = ctx.Done()
	)
	wait := func(extra time.Duration) bool {
		mu.Lock()
		now := time.Now()
		at := now.Add(extra)
		if next.After(at) {
			at = next
		}
		next = at.Add(pace)
		mu.Unlock()
		if d := time.Until(at); d > 0 {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
			case <-canceled:
				return false
			}
		}
		return true
	}
	worker := func() {
		defer wg.Done()
		for port := range jobs {
			var extra time.Duration
			for attempt := 0; ; attempt++ {
				if !wait(extra) {
					break
				}
				dctx, cancel := context.WithTimeout(ctx, dialTO)
				conn, err := nw.DialTCP(dctx, tc, d, host, port, ports)
				cancel()
				if err == nil {
					conn.Close()
					mu.Lock()
					openSet[port] = true
					mu.Unlock()
					break
				}
				var re *RefusedError
				if errors.As(err, &re) && (re.Reason == "rate_limited" || re.Reason == "concurrency_limited") && retries > 0 && attempt < retries {
					tc.RetryingEdgeLimit()
					extra = min(max(re.RetryAfter, pace), 5*time.Second)
					continue
				}
				if errors.As(err, &re) && re.Reason != "connect_failed" && re.Reason != "" {
					_ = out.Skipped(net.JoinHostPort(host, strconv.Itoa(port)), "egress_refused:"+re.Reason)
				}
				break
			}
			mu.Lock()
			done++
			n := done
			mu.Unlock()
			_ = out.Progress(n, int64(len(ports)), "")
		}
	}
	for range min(par, max(1, len(ports))) {
		wg.Add(1)
		go worker()
	}
feed:
	for _, port := range ports {
		select {
		case jobs <- port:
		case <-canceled:
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	for _, port := range ports {
		if openSet[port] {
			open = append(open, port)
		}
	}
	if len(open) > 0 {
		obs := oam.Observation{ObservationID: tc.ObservationID(p.Spec().Name, host, fmt.Sprint(ports)), ObservedAt: tc.Now(), Confidence: 0.9,
			Entities: []oam.ObservedEntity{{Ref: "h", Asset: asset}}}
		for _, port := range open {
			ref := "s" + strconv.Itoa(port)
			obs.Entities = append(obs.Entities, oam.ObservedEntity{Ref: ref, Asset: oam.Service{UniqueID: oam.ServiceUniqueID(key, port, "tcp", ""), ServiceType: "tcp"}})
			obs.Edges = append(obs.Edges, oam.ObservedEdge{From: "h", To: ref, Relation: oam.PortRelation{Label: oam.LabelPort, PortNumber: port, Protocol: "tcp"}})
		}
		if err := out.Observation(obs); err != nil {
			return nil, err
		}
	}
	return json.Marshal(open)
}

// TLSCert is the tlscert tool (V2_DESIGN §8; passive per operator decision
// D3): a TLS handshake through a CONNECT tunnel under a pinned decision,
// with SNI = host. Verification is skipped only to read the presented
// chain: the handshake is aborted as soon as the server's certificates
// arrive, so no application data is ever sent. It emits the leaf as a
// TLSCertificate with a certificate edge from the Service and common_name /
// san_dns_name edges to FQDNs.
type TLSCert struct {
	// Timeout bounds the connection and handshake (default 10 s).
	Timeout time.Duration
}

// Spec implements Adapter.
func (TLSCert) Spec() Spec {
	return Spec{Name: "tlscert", Version: "1.0.0", Description: "Read the TLS certificate a service presents (handshake only)",
		InputType: "(InScope<Host>, Int)", OutputType: "Option<TlsCertificate>",
		Observes: []string{oam.TypeTLSCertificate, oam.TypeService, oam.TypeFQDN, oam.TypeIPAddress},
		Class:    core.ClassPassive, RateRPS: 5, RateBurst: 10, Concurrency: 4, Timeout: time.Minute, Idempotent: true,
		Idempotency: engine.IdempotencyIdempotent, Retry: RetryTransient}
}

// errChainRead aborts the handshake once the chain is captured.
var errChainRead = errors.New("tlscert: chain read")

// Run implements Adapter.
func (t TLSCert) Run(ctx context.Context, input json.RawMessage, tc *ToolCtx, out Emitter) (json.RawMessage, error) {
	args, err := decodeArgs(input, 2)
	if err != nil {
		return nil, err
	}
	host, err := decodeHost(args[0])
	if err != nil {
		return nil, err
	}
	var port int
	if err := json.Unmarshal(args[1], &port); err != nil || port < 1 || port > 65535 {
		return nil, Errorf("invalid_input", false, "port must be an integer in 1..65535")
	}
	nw := tc.Network()
	if nw == nil {
		return nil, ErrNoEgress
	}
	d, ok, err := tc.AllowedPinned(ctx, out, host, scope.TargetHost, core.ClassPassive)
	if err != nil {
		return nil, err
	}
	if !ok {
		return json.RawMessage("null"), nil
	}
	asset, key, err := hostAsset(host)
	if err != nil {
		return nil, Errorf("invalid_input", false, err.Error())
	}
	to := t.Timeout
	if to <= 0 {
		to = 10 * time.Second
	}
	hctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	conn, err := nw.DialTCP(hctx, tc, d, host, port, []int{port})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var re *RefusedError
		if errors.As(err, &re) && re.Reason != "connect_failed" {
			_ = out.Skipped(net.JoinHostPort(host, strconv.Itoa(port)), "egress_refused:"+re.Reason)
		}
		return json.RawMessage("null"), nil
	}
	defer conn.Close()
	// Record what the server sends during the handshake (G-SVC-16).
	conn = tc.Raw.Conn(conn, RawTLSHandshake, "tls:"+net.JoinHostPort(host, strconv.Itoa(port)))
	var chain [][]byte
	cfg := &tls.Config{
		InsecureSkipVerify: true, // only to read the chain; the handshake is aborted below
		MinVersion:         tls.VersionTLS10,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			chain = raw
			return errChainRead
		},
	}
	if _, isIP := asset.(oam.IPAddress); !isIP {
		cfg.ServerName = host
	}
	tconn := tls.Client(conn, cfg)
	err = tconn.HandshakeContext(hctx)
	if len(chain) == 0 {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		_ = out.Progress(1, 1, "no certificate: "+fmt.Sprint(err))
		return json.RawMessage("null"), nil
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return nil, Errorf("invalid_response", false, "the presented certificate does not parse: "+err.Error())
	}
	obs := oam.Observation{ObservationID: tc.ObservationID("tlscert", host, strconv.Itoa(port)), ObservedAt: tc.Now(), Confidence: 0.95,
		Entities: []oam.ObservedEntity{
			{Ref: "h", Asset: asset},
			{Ref: "s", Asset: oam.Service{UniqueID: oam.ServiceUniqueID(key, port, "tcp", "tls"), ServiceType: "tls"}},
		},
		Edges: []oam.ObservedEdge{
			{From: "h", To: "s", Relation: oam.PortRelation{Label: oam.LabelPort, PortNumber: port, Protocol: "tcp"}},
			{From: "s", To: "c", Relation: oam.SimpleRelation{Label: oam.LabelCertificate}},
		}}
	a := certObservation(&obs, "c", leaf)
	if err := out.Observation(obs); err != nil {
		return nil, err
	}
	_ = out.Progress(1, 1, "certificate read")
	return json.Marshal(certRecord(a))
}
