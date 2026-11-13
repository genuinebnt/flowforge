package toolrunner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/scope"
)

// CTEntry is one member of crt.sh's JSON search output. name_value holds
// the certificate's names separated by newlines.
type CTEntry struct {
	ID             int64  `json:"id"`
	IssuerCAID     int64  `json:"issuer_ca_id"`
	IssuerName     string `json:"issuer_name"`
	CommonName     string `json:"common_name"`
	NameValue      string `json:"name_value"`
	EntryTimestamp string `json:"entry_timestamp"`
	NotBefore      string `json:"not_before"`
	NotAfter       string `json:"not_after"`
	SerialNumber   string `json:"serial_number"`
}

// MaxCTBody bounds a crt.sh search response.
const MaxCTBody = 32 << 20

// ParseCTLogJSON decodes a crt.sh search response (a JSON array; crt.sh
// answers an empty result as [] or, historically, as an empty body).
func ParseCTLogJSON(b []byte) ([]CTEntry, error) {
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil, nil
	}
	var es []CTEntry
	if err := json.Unmarshal(b, &es); err != nil {
		return nil, fmt.Errorf("ctlog: %w", err)
	}
	return es, nil
}

// CTNames returns the normalized names of an entry: name_value split on
// newlines plus common_name, wildcards replaced by their base name,
// non-DNS values (e-mail addresses, IPs) dropped, sorted and deduplicated.
func CTNames(e CTEntry) []string {
	var out []string
	for _, s := range append(strings.Split(e.NameValue, "\n"), e.CommonName) {
		if n, ok := certName(s); ok {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// within reports whether n is domain or below it.
func within(n, domain string) bool { return n == domain || strings.HasSuffix(n, "."+domain) }

// CTLog is the passive ctlog tool (spec 6.4 "Certificate transparency
// query", V2_DESIGN §8): it searches crt.sh (an allowlisted intelligence
// source, reached through the egress proxy under the input's passive
// decision) for certificates of the name and its subdomains. It returns
// the names at or below the input; for each certificate (newest first, at
// most MaxCertificates) it downloads the DER, because a TLSCertificate is
// keyed by its SHA-256 fingerprint, and emits it with common_name and
// san_dns_name edges.
type CTLog struct {
	// BaseURL defaults to https://crt.sh.
	BaseURL string
	// MaxCertificates bounds certificate downloads per call (default 50).
	MaxCertificates int
}

// Spec implements Adapter.
func (*CTLog) Spec() Spec {
	return Spec{Name: "ctlog", Version: "1.0.0", Description: "Certificate transparency search (crt.sh)",
		InputType: "Fqdn", OutputType: "List<Fqdn>", Observes: []string{oam.TypeTLSCertificate, oam.TypeFQDN},
		Class: core.ClassPassive, RateRPS: 1, RateBurst: 5, Concurrency: 1, Timeout: 3 * time.Minute, Idempotent: true}
}

// Run implements Adapter.
func (c *CTLog) Run(ctx context.Context, input json.RawMessage, tc *ToolCtx, out Emitter) (json.RawMessage, error) {
	name, err := decodeHost(input)
	if err != nil {
		return nil, err
	}
	t, err := scope.Normalize(name, scope.TargetHost)
	if err != nil || !t.IsName() {
		return nil, Errorf("invalid_input", false, "input is not a DNS name")
	}
	name = t.Name()
	nw := tc.Network()
	if nw == nil {
		return nil, ErrNoEgress
	}
	d, ok, err := tc.Allowed(ctx, out, name, scope.TargetHost, core.ClassPassive)
	if !ok {
		if err != nil {
			return nil, err
		}
		return json.RawMessage("[]"), nil
	}
	base := strings.TrimSuffix(c.BaseURL, "/")
	if base == "" {
		base = "https://crt.sh"
	}
	hc := nw.IntelClient(tc, d)
	body, err := ctGet(ctx, hc, base+"/?q="+url.QueryEscape("%."+name)+"&output=json", MaxCTBody)
	if err != nil {
		return nil, err
	}
	entries, err := ParseCTLogJSON(body)
	if err != nil {
		return nil, Errorf("invalid_response", false, err.Error())
	}
	// Names at or below the input, over every entry.
	var names []string
	for _, e := range entries {
		for _, n := range CTNames(e) {
			if within(n, name) {
				names = append(names, n)
			}
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	// Certificates: unique ids, newest (highest id) first.
	ids := map[int64]bool{}
	var certIDs []int64
	for _, e := range entries {
		if e.ID > 0 && !ids[e.ID] {
			ids[e.ID] = true
			certIDs = append(certIDs, e.ID)
		}
	}
	sort.Slice(certIDs, func(i, j int) bool { return certIDs[i] > certIDs[j] })
	max := c.MaxCertificates
	if max <= 0 {
		max = 50
	}
	if len(certIDs) > max {
		certIDs = certIDs[:max]
	}
	total := int64(len(certIDs) + 1)
	for i, id := range certIDs {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		_ = out.Progress(int64(i), total, "certificates")
		der, err := ctGet(ctx, hc, base+"/?d="+strconv.FormatInt(id, 10), 1<<20)
		if err != nil {
			if te, ok := isToolError(err); ok && te.Retryable {
				_ = out.Skipped("crt.sh certificate "+strconv.FormatInt(id, 10), "unavailable")
				continue
			}
			return nil, err
		}
		cert, err := parseCertificate(der)
		if err != nil {
			_ = out.Skipped("crt.sh certificate "+strconv.FormatInt(id, 10), "invalid_certificate")
			continue
		}
		obs := oam.Observation{ObservationID: tc.ObservationID("ctlog", name, "cert", strconv.FormatInt(id, 10)), ObservedAt: tc.Now(), Confidence: 0.9}
		certObservation(&obs, "c", cert)
		if len(obs.Entities) > oam.MaxBatchEntities {
			obs.Entities = obs.Entities[:oam.MaxBatchEntities]
			obs.Edges = slices.DeleteFunc(obs.Edges, func(e oam.ObservedEdge) bool { return !hasRef(obs.Entities, e.To) })
		}
		if err := out.Observation(obs); err != nil {
			return nil, err
		}
	}
	for i := 0; i < len(names); i += oam.MaxBatchEntities {
		chunk := names[i:min(i+oam.MaxBatchEntities, len(names))]
		obs := oam.Observation{ObservationID: tc.ObservationID("ctlog", name, "names", strconv.Itoa(i)), ObservedAt: tc.Now(), Confidence: 0.8}
		for j, n := range chunk {
			obs.Entities = append(obs.Entities, oam.ObservedEntity{Ref: "n" + strconv.Itoa(j), Asset: oam.FQDN{Name: n}})
		}
		if err := out.Observation(obs); err != nil {
			return nil, err
		}
	}
	_ = out.Progress(total, total, fmt.Sprintf("%d names, %d certificates", len(names), len(certIDs)))
	if names == nil {
		names = []string{}
	}
	return json.Marshal(names)
}

func hasRef(es []oam.ObservedEntity, ref string) bool {
	for _, e := range es {
		if e.Ref == ref {
			return true
		}
	}
	return false
}

// ctGet fetches u with an intelligence-source client.
func ctGet(ctx context.Context, hc *http.Client, u string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "flowforge-ctlog/1.0")
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, Errorf("unavailable", true, "crt.sh request failed: "+err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return nil, Errorf("unavailable", true, "crt.sh answered "+resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, Errorf("invalid_response", false, "crt.sh answered "+resp.Status)
	}
	b, err := readLimited(resp, limit)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func readLimited(resp *http.Response, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, Errorf("unavailable", true, "reading the response: "+err.Error())
	}
	if int64(len(b)) > limit {
		return nil, Errorf("invalid_response", false, "response too large")
	}
	return b, nil
}
