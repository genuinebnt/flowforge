package toolrunner_test

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/dnsmsg"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/services/egressproxy"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
	"github.com/genuinebnt/flowforge/services/toolrunner"
)

// wireDNS is dnsNet that also records each answer as the wire message a
// forwarder would have sent (Egress.LookupDNS does that for real queries).
type wireDNS struct{ dnsNet }

func (n *wireDNS) LookupDNS(ctx context.Context, tc *toolrunner.ToolCtx, d scope.Decision, name string, t dnsmsg.Type) (egressproxy.DNSAnswer, error) {
	ans, err := n.dnsNet.LookupDNS(ctx, tc, d, name, t)
	if err != nil {
		return ans, err
	}
	m := dnsmsg.NewQuery(1, name, t)
	m.Header.Response, m.Answers = true, ans.Answers
	b, err := dnsmsg.Encode(m)
	if err != nil {
		return ans, err
	}
	tc.Raw.Add(toolrunner.RawDNSMessage, "dns:"+name, b)
	return ans, nil
}

// TestRegress_GOPS14_RawOutputToBlobStore: with a raw store, raw tool
// output reaches object storage and each observation's raw_ref names it
// (spec 10.5 graph.observations.raw_ref); the blob store adapter keeps one
// object per distinct transcript, named by its hash under the tenant and
// day, so the same effect run twice with the same bytes writes the same
// object. (Since G-SVC-16 the object is the FFRW transcript of the
// protocol bytes, not the observation.)
func TestRegress_GOPS14_RawOutputToBlobStore(t *testing.T) {
	blobs := &graphstore.MemBlobs{}
	nw := &wireDNS{dnsNet{answers: map[string]string{"app.test.": "127.0.0.1"}}}
	c, ctx := runnerClient(t, toolrunner.Options{Adapters: []toolrunner.Adapter{toolrunner.DNSResolve{}}, Net: nw,
		Raw: toolrunner.BlobRaw{Blobs: blobs}})
	refs := map[string]bool{}
	for range 2 {
		for _, m := range execute(t, c, ctx, "dnsresolve", "app.test") {
			if o := m.GetObservation(); o != nil {
				refs[o.GetRawRef()] = true
			}
		}
	}
	if len(refs) != 1 {
		t.Fatalf("raw refs %v, want one object for the repeated effect", refs)
	}
	for ref := range refs {
		if !strings.HasPrefix(ref, "mem://raw/"+servicetest.TenantA.String()+"/") || !strings.HasSuffix(ref, ".ffrw") {
			t.Fatalf("raw_ref %q", ref)
		}
		data, err := blobs.Get(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		tr, err := toolrunner.DecodeRawTranscript(data)
		if err != nil || len(tr.Records) != 5 { // A, AAAA, MX, NS, TXT
			t.Fatalf("raw object: %+v %v", tr, err)
		}
		m, _, err := dnsmsg.Decode(tr.Records[0].Data, dnsmsg.Strict)
		if err != nil || len(m.Answers) != 1 || m.Answers[0].Data.IP != netip.MustParseAddr("127.0.0.1") {
			t.Fatalf("first record is not the A answer: %+v %v", m, err)
		}
	}
}
