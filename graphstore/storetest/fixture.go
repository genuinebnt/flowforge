package storetest

import (
	"strings"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/oam"
)

// fixture is the traversal and search graph:
//
//	org -owns-> api, jenkins          (asnmap, 0.6, T0+2m, run r3)
//	www -dns rr5-> api                (dnsx, 0.9, T0, run r1)
//	api -dns rr1-> ip1, jenkins -dns rr1-> ip2
//	ip1 -port 443-> svc, ip2 -port 8080-> svc2   (httpx, 0.8, T0+1m, run r2)
//	net -contains-> ip1, ip2          (asnmap, 0.6, T0+2m, run r3)
//	www -related_to-> ip1             (guess, 0.3, T0+1m)
//	old                               (subfinder, 0.5, T0+3m, run r4; jenkins seen again)
type fixture struct {
	ids  map[string]string // ref -> entity id
	runs [4]string
}

func fixtureObservations() []graphstore.IngestRequest {
	fqdn := func(n string) oam.Asset { return oam.FQDN{Name: n} }
	ip := func(a string) oam.Asset { return oam.IPAddress{Address: a, IPVersion: "IPv4"} }
	o1 := obs("fx-1", "dnsx", T0, 0.9, []oam.ObservedEntity{
		ent("api", fqdn("api.acme.com")), ent("jenkins", fqdn("jenkins.acme.com")), ent("www", fqdn("www.acme.com")),
		ent("ip1", ip("203.0.113.10")), ent("ip2", ip("203.0.113.20")),
	}, dns("api", "ip1", 1), dns("jenkins", "ip2", 1), dns("www", "api", 5))
	o2 := obs("fx-2", "httpx", T0.Add(time.Minute), 0.8, []oam.ObservedEntity{
		ent("ip1", ip("203.0.113.10")), ent("ip2", ip("203.0.113.20")),
		ent("svc", oam.Service{UniqueID: "svc-443", ServiceType: "https", Output: "nginx welcome"}),
		ent("svc2", oam.Service{UniqueID: "svc-8080", ServiceType: "http", Output: "Jenkins ready"}),
	}, port("ip1", "svc", 443), port("ip2", "svc2", 8080))
	o2b := obs("fx-2b", "guess", T0.Add(time.Minute), 0.3, []oam.ObservedEntity{
		ent("www", fqdn("www.acme.com")), ent("ip1", ip("203.0.113.10")),
	}, simple("www", "ip1", "related_to"))
	o3 := obs("fx-3", "asnmap", T0.Add(2*time.Minute), 0.6, []oam.ObservedEntity{
		ent("net", oam.Netblock{CIDR: "203.0.113.0/24", IPVersion: "IPv4"}),
		ent("ip1", ip("203.0.113.10")), ent("ip2", ip("203.0.113.20")),
		ent("org", oam.Organization{UniqueID: "acme", Name: "Acme"}),
		ent("api", fqdn("api.acme.com")), ent("jenkins", fqdn("jenkins.acme.com")),
	}, simple("net", "ip1", "contains"), simple("net", "ip2", "contains"), simple("org", "api", "owns"), simple("org", "jenkins", "owns"))
	o4 := obs("fx-4", "subfinder", T0.Add(3*time.Minute), 0.5, []oam.ObservedEntity{
		ent("old", fqdn("old.acme.com")), ent("jenkins", fqdn("jenkins.acme.com")),
	})
	runs := fixtureRuns()
	reqs := []graphstore.IngestRequest{
		{RunID: runs[0], Observation: o1}, {RunID: runs[1], Observation: o2}, {Observation: o2b},
		{RunID: runs[2], Observation: o3}, {RunID: runs[3], Observation: o4},
	}
	return reqs
}

func fixtureRuns() [4]string {
	return [4]string{RunID("fx-r1"), RunID("fx-r2"), RunID("fx-r3"), RunID("fx-r4")}
}

// fixtureScope puts *.acme.com in scope except old.acme.com.
func fixtureScope(_, _ string, a oam.Asset) oam.ScopeStatus {
	f, ok := a.(oam.FQDN)
	switch {
	case !ok:
		return oam.ScopeUnknown
	case f.Name == "old.acme.com":
		return oam.ScopeOutOfScope
	case strings.HasSuffix(f.Name, ".acme.com"):
		return oam.ScopeInScope
	}
	return oam.ScopeUnknown
}

// load ingests the fixture into e's tenant and program.
func (e *env) load() fixture {
	e.t.Helper()
	fx := fixture{ids: map[string]string{}, runs: fixtureRuns()}
	for _, req := range fixtureObservations() {
		req.TenantID, req.ProgramID = e.tenant, e.prog
		res, err := e.s.IngestObservation(e.ctx, req)
		if err != nil {
			e.t.Fatalf("fixture %s: %v", req.Observation.ObservationID, err)
		}
		for ref, id := range res.RefToEntityID {
			fx.ids[ref] = id
		}
	}
	return fx
}

func newFixtureEnv(t testing.TB, f Factory) (*env, fixture) {
	e := newEnv(t, f, Options{Scope: fixtureScope})
	return e, e.load()
}

func entityIDs(ents []graphstore.Entity) []string {
	out := make([]string, len(ents))
	for i, e := range ents {
		out[i] = e.ID
	}
	return out
}

// refs maps entity ids back to fixture refs, for readable failures.
func (fx fixture) refs(ids []string) []string {
	back := map[string]string{}
	for ref, id := range fx.ids {
		back[id] = ref
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		if r, ok := back[id]; ok {
			out[i] = r
		} else {
			out[i] = id
		}
	}
	return out
}

func (fx fixture) sorted(refs ...string) []string {
	ids := make([]string, len(refs))
	for i, r := range refs {
		ids[i] = fx.ids[r]
	}
	sortStrings(ids)
	return fx.refs(ids)
}
