package wellknown

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

var unmarshal = json.Unmarshal

// --- reference implementation of RFC 9309 semantics ----------------------
//
// Written independently of robots.go and as directly as possible from the
// RFC text, trading speed for obviousness (exponential wildcard matching).

type mrule struct {
	allow   bool
	pattern string // as written
}

type mgroup struct {
	agents []string // as written
	rules  []mrule
}

func refToken(ua string) string {
	ua = strings.TrimSpace(ua)
	if strings.HasPrefix(ua, "*") {
		return "*"
	}
	end := strings.IndexFunc(ua, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' || r == '-')
	})
	if end < 0 {
		end = len(ua)
	}
	return strings.ToLower(ua[:end])
}

func refNormalize(s string) string {
	var out []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '%' && i+2 < len(s) && strings.ContainsRune("0123456789abcdefABCDEF", rune(s[i+1])) &&
			strings.ContainsRune("0123456789abcdefABCDEF", rune(s[i+2])) {
			var v byte
			fmt.Sscanf(s[i+1:i+3], "%02x", &v)
			if strings.IndexByte("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~", v) >= 0 {
				out = append(out, v)
			} else {
				out = append(out, fmt.Sprintf("%%%02X", v)...)
			}
			i += 2
			continue
		}
		if c <= ' ' || c >= 0x7f {
			out = append(out, fmt.Sprintf("%%%02X", c)...)
			continue
		}
		out = append(out, c)
	}
	return string(out)
}

// refMatch tries every way "*" can split the path.
func refMatch(p, s string, anchored bool) bool {
	if p == "" {
		return !anchored || s == ""
	}
	if p[0] == '*' {
		for k := 0; k <= len(s); k++ {
			if refMatch(p[1:], s[k:], anchored) {
				return true
			}
		}
		return false
	}
	return s != "" && p[0] == s[0] && refMatch(p[1:], s[1:], anchored)
}

func refRuleMatches(pattern, path string) bool {
	anchored := strings.HasSuffix(pattern, "$")
	if anchored {
		pattern = pattern[:len(pattern)-1]
	}
	return refMatch(pattern, path, anchored)
}

func refAllowed(model []mgroup, ua, path string) bool {
	if path == "" {
		path = "/"
	}
	path = refNormalize(path)
	if path == "/robots.txt" {
		return true
	}
	tok := refToken(ua)
	var chosen []mgroup
	for _, g := range model {
		for _, a := range g.agents {
			if tok != "*" && tok != "" && refToken(a) == tok {
				chosen = append(chosen, g)
				break
			}
		}
	}
	if len(chosen) == 0 {
		for _, g := range model {
			for _, a := range g.agents {
				if refToken(a) == "*" {
					chosen = append(chosen, g)
					break
				}
			}
		}
	}
	bestLen, bestAllow, found := -1, false, false
	for _, g := range chosen {
		for _, r := range g.rules {
			p := refNormalize(r.pattern)
			if !refRuleMatches(p, path) {
				continue
			}
			if len(p) > bestLen || (len(p) == bestLen && r.allow) {
				bestLen, bestAllow, found = len(p), r.allow, true
			}
		}
	}
	return !found || bestAllow
}

// --- generators ----------------------------------------------------------

var (
	gAgents    = []string{"*", "foobot", "FooBot/2.1", "barbot", "baz_bot", "news-bot", "* (all)"}
	gPathParts = []string{"/", "a", "b", "/a", "/b", "*", "$", "%61", "%2F", "%2f", "é", "?", "=", "x.gif", "%7E"}
	gCrawlers  = []string{"foobot", "FooBot", "barbot", "baz_bot", "news-bot", "other", "foobot/1.0 (+https://x)"}
)

func genPath(r core.Rng, pattern bool) string {
	var b strings.Builder
	b.WriteString("/")
	for i := core.IntN(r, 5); i > 0; i-- {
		p := gPathParts[core.IntN(r, len(gPathParts))]
		if !pattern && (p == "*" || p == "$") {
			continue
		}
		b.WriteString(p)
	}
	if pattern && core.IntN(r, 5) == 0 {
		b.WriteString("$")
	}
	return b.String()
}

// genRobots returns a model and a robots.txt text for it.
func genRobots(r core.Rng) ([]mgroup, []byte) {
	var model []mgroup
	var b bytes.Buffer
	nl := []string{"\n", "\r\n", "\n\n", "\r"}[core.IntN(r, 4)]
	key := func(k string) string {
		switch core.IntN(r, 3) {
		case 0:
			return strings.ToUpper(k)
		case 1:
			return k
		}
		return strings.ToUpper(k[:1]) + k[1:]
	}
	for gi := core.IntN(r, 5); gi > 0; gi-- {
		var g mgroup
		for ai := core.IntN(r, 2) + 1; ai > 0; ai-- {
			a := gAgents[core.IntN(r, len(gAgents))]
			g.agents = append(g.agents, a)
			fmt.Fprintf(&b, "%s:%s%s%s", key("user-agent"), []string{"", " ", "\t "}[core.IntN(r, 3)], a, nl)
			if core.IntN(r, 4) == 0 {
				b.WriteString("# comment" + nl)
			}
		}
		n := core.IntN(r, 5)
		if n == 0 {
			b.WriteString(key("disallow") + ":" + nl) // ends the agent run
		}
		for ; n > 0; n-- {
			ru := mrule{allow: core.IntN(r, 2) == 0, pattern: genPath(r, true)}
			g.rules = append(g.rules, ru)
			k := "disallow"
			if ru.allow {
				k = "allow"
			}
			fmt.Fprintf(&b, "%s: %s%s%s", key(k), ru.pattern, []string{"", " # why", "  "}[core.IntN(r, 3)], nl)
			if core.IntN(r, 6) == 0 {
				b.WriteString("Sitemap: https://example.com/s.xml" + nl)
			}
		}
		model = append(model, g)
	}
	return model, b.Bytes()
}

// --- invariants ----------------------------------------------------------

// TestWKInv01_RobotsMatchesReference: on generated robots.txt files the
// parser + matcher agree with the reference implementation of RFC 9309
// (group selection, merging, percent normalization, wildcards, "$",
// longest match, allow on ties, implicit /robots.txt) for random crawlers
// and paths.
func TestWKInv01_RobotsMatchesReference(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases(); i++ {
		model, text := genRobots(r)
		rb := ParseRobots(text, RobotsOptions{})
		for k := 0; k < 30; k++ {
			ua := gCrawlers[core.IntN(r, len(gCrawlers))]
			path := genPath(r, false)
			if core.IntN(r, 20) == 0 {
				path = "/robots.txt"
			}
			if got, want := rb.Allowed(ua, path), refAllowed(model, ua, path); got != want {
				t.Fatalf("robots:\n%s\nAllowed(%q, %q) = %v, reference %v", text, ua, path, got, want)
			}
		}
	}
}

// TestWKInv02_PatternMatcherMatchesBruteForce: the linear-backtracking
// matcher equals exhaustive wildcard matching.
func TestWKInv02_PatternMatcherMatchesBruteForce(t *testing.T) {
	r := testkit.Rng(t)
	alpha := []string{"a", "b", "*", "/", "$"}
	for i := 0; i < testkit.Cases()*20; i++ {
		var p, s strings.Builder
		for k := core.IntN(r, 7); k > 0; k-- {
			p.WriteString(alpha[core.IntN(r, len(alpha))])
		}
		for k := core.IntN(r, 8); k > 0; k-- {
			s.WriteString(alpha[core.IntN(r, 4)])
		}
		if got, want := matchPattern(p.String(), s.String()), refRuleMatches(p.String(), s.String()); got != want {
			t.Fatalf("match(%q, %q) = %v, brute force %v", p.String(), s.String(), got, want)
		}
	}
}

// TestWKInv03_Totality: any byte string parses as robots.txt and as
// security.txt; diagnostics carry line numbers within the input.
func TestWKInv03_Totality(t *testing.T) {
	r := testkit.Rng(t)
	frags := []string{"User-agent:", "Disallow:", "Allow:", " /a*", "$", "#", ":", "\n", "\r", "\r\n", "Contact:",
		"Expires:", "2030-01-01T00:00:00Z", "mailto:x@y", "-----BEGIN PGP SIGNED MESSAGE-----", "-----BEGIN PGP SIGNATURE-----",
		"-----END PGP SIGNATURE-----", "- ", "\xff", "\x00", "Sitemap:", "Crawl-delay: 1.5", "https://e.example/"}
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < testkit.Cases(); i++ {
		var b bytes.Buffer
		for k := core.IntN(r, 40); k > 0; k-- {
			if core.IntN(r, 8) == 0 {
				b.WriteByte(byte(r.Uint64()))
			} else {
				b.WriteString(frags[core.IntN(r, len(frags))])
			}
		}
		src := b.Bytes()
		lines := bytes.Count(src, []byte("\n")) + bytes.Count(src, []byte("\r")) + 1
		rb := ParseRobots(src, RobotsOptions{})
		for _, d := range rb.Diags {
			if d.Line < 0 || d.Line > lines {
				t.Fatalf("robots diag line %d of %d", d.Line, lines)
			}
		}
		for _, g := range rb.Groups {
			if len(g.Agents) == 0 {
				t.Fatalf("group without agents")
			}
		}
		rb.Allowed("x", "/a")
		s := ParseSecurityTxt(src, now, SecurityOptions{})
		for _, d := range s.Diags {
			if d.Line < 0 || d.Line > lines {
				t.Fatalf("security diag line %d of %d", d.Line, lines)
			}
		}
	}
}

// TestWKInv04_LimitsGradePartial: hitting the byte or item limit grades
// robots.txt and security.txt partial, never complete; within the limits
// they are complete, and no more items than the limit are kept.
func TestWKInv04_LimitsGradePartial(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases(); i++ {
		_, text := genRobots(r)
		full := ParseRobots(text, RobotsOptions{})
		items := len(full.Sitemaps)
		for _, g := range full.Groups {
			items += len(g.Rules)
		}
		maxItems, maxBytes := core.IntN(r, 8)+1, core.IntN(r, len(text)+20)+1
		rb := ParseRobots(text, RobotsOptions{MaxItems: maxItems, MaxBytes: maxBytes})
		hit := false
		for _, d := range rb.Diags {
			hit = hit || d.Code == RobotsLimitExceeded || d.Code == RobotsInputTooLarge
		}
		if hit != (rb.Grade == GradePartial) {
			t.Fatalf("limit %v grade %s", hit, rb.Grade)
		}
		if (len(text) > maxBytes || items > maxItems) && rb.Grade != GradePartial {
			t.Fatalf("over a limit but %s", rb.Grade)
		}
		kept := len(rb.Sitemaps)
		for _, g := range rb.Groups {
			kept += len(g.Rules)
		}
		if kept > maxItems {
			t.Fatalf("kept %d items over %d", kept, maxItems)
		}
		body := genSecurity(r)
		s := ParseSecurityTxt(body, time.Unix(0, 0), SecurityOptions{MaxItems: maxItems, MaxBytes: core.IntN(r, len(body)+20) + 1})
		shit := false
		for _, d := range s.Diags {
			shit = shit || d.Code == SecurityLimitExceeded || d.Code == SecurityInputTooLarge
		}
		if shit != (s.Grade == GradePartial) || len(s.Fields) > maxItems {
			t.Fatalf("security.txt limit %v grade %s fields %d", shit, s.Grade, len(s.Fields))
		}
	}
}

func genSecurity(r core.Rng) []byte {
	var b bytes.Buffer
	for k := core.IntN(r, 4) + 1; k > 0; k-- {
		fmt.Fprintf(&b, "Contact: mailto:sec%d@example.com\n", k)
	}
	fmt.Fprintf(&b, "Expires: 20%02d-0%d-15T10:00:00Z\n", 20+core.IntN(r, 20), core.IntN(r, 9)+1)
	if core.IntN(r, 2) == 0 {
		b.WriteString("Policy: https://example.com/policy\n# comment\nPreferred-Languages: en, de\n")
	}
	if core.IntN(r, 2) == 0 {
		b.WriteString("-Dash-Field: kept\n")
	}
	return b.Bytes()
}

// TestWKInv05_ExpiresAgainstArgument: the expired verdict depends only on
// the time argument: expired exactly when Expires <= now, and the same
// input and time give the same result.
func TestWKInv05_ExpiresAgainstArgument(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases(); i++ {
		body := genSecurity(r)
		base := ParseSecurityTxt(body, time.Unix(0, 0), SecurityOptions{})
		exp := base.Expires
		for _, now := range []time.Time{exp.Add(-time.Second), exp, exp.Add(time.Nanosecond), exp.Add(-time.Duration(core.IntN(r, 1e6)) * time.Hour)} {
			s := ParseSecurityTxt(body, now, SecurityOptions{})
			expired := false
			for _, d := range s.Diags {
				expired = expired || d.Code == SecurityExpired
			}
			if expired != !exp.After(now) {
				t.Fatalf("expires %v now %v: expired %v", exp, now, expired)
			}
			again := ParseSecurityTxt(body, now, SecurityOptions{})
			if fmt.Sprint(again) != fmt.Sprint(s) {
				t.Fatalf("not deterministic")
			}
		}
	}
}

// TestWKInv06_SignedFramingTransparent: wrapping a security.txt in an
// OpenPGP cleartext signature (RFC 4880 §7, with dash-escaping) yields
// the same fields and diagnostics, with Signed set.
func TestWKInv06_SignedFramingTransparent(t *testing.T) {
	r := testkit.Rng(t)
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < testkit.Cases(); i++ {
		body := genSecurity(r)
		var signed bytes.Buffer
		signed.WriteString("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\n")
		for _, line := range strings.SplitAfter(string(body), "\n") {
			if strings.HasPrefix(line, "-") {
				signed.WriteString("- ")
			}
			signed.WriteString(line)
		}
		signed.WriteString("-----BEGIN PGP SIGNATURE-----\n\niQEzBAEBCAAd\n=XyZ1\n-----END PGP SIGNATURE-----\n")
		plain := ParseSecurityTxt(body, now, SecurityOptions{})
		wrapped := ParseSecurityTxt(signed.Bytes(), now, SecurityOptions{})
		if !wrapped.Signed || plain.Signed {
			t.Fatalf("signed flags %v %v", plain.Signed, wrapped.Signed)
		}
		pf, wf := fieldsOf(plain), fieldsOf(wrapped)
		if pf != wf || codes(plain) != codes(wrapped) {
			t.Fatalf("plain %s %s\nsigned %s %s", pf, codes(plain), wf, codes(wrapped))
		}
	}
}

func fieldsOf(s *SecurityTxt) string {
	var b strings.Builder
	for _, f := range s.Fields {
		b.WriteString(f.Name + "=" + f.Value + ";")
	}
	return b.String()
}

func codes(s *SecurityTxt) string {
	var b strings.Builder
	for _, d := range s.Diags {
		b.WriteString(d.Code.String() + ",")
	}
	return b.String()
}

// TestWKInv07_LinearParse: parsing allocates linearly (at 4n at most 4.4x
// the allocations at n) and, outside -short/-race, takes per-byte time
// within 1.5x.
func TestWKInv07_LinearParse(t *testing.T) {
	gen := func(n int) []byte {
		rr := core.NewSplitMix64(0x524f424f) // "ROBO"
		var b bytes.Buffer
		for b.Len() < n {
			_, text := genRobots(rr)
			b.Write(text)
			b.WriteString("\n")
		}
		return b.Bytes()
	}
	small, big := gen(100<<10), gen(400<<10)
	opts := RobotsOptions{MaxBytes: 1 << 20, MaxItems: 1 << 20}
	as := testing.AllocsPerRun(2, func() { ParseRobots(small, opts) })
	ab := testing.AllocsPerRun(2, func() { ParseRobots(big, opts) })
	if ab > 4.4*as*float64(len(big))/float64(4*len(small)) {
		t.Fatalf("allocations %.0f -> %.0f", as, ab)
	}
	if testing.Short() || raceEnabled {
		t.Skip("timing half skipped in -short and -race runs")
	}
	per := func(b []byte) float64 {
		best := time.Duration(1 << 62)
		for i := 0; i < 5; i++ {
			s := time.Now()
			ParseRobots(b, opts)
			best = min(best, time.Since(s))
		}
		return float64(best) / float64(len(b))
	}
	for attempt := 0; ; attempt++ {
		a, b := per(small), per(big)
		if b <= 1.5*a+1 {
			return
		}
		if attempt == 2 {
			t.Fatalf("per-byte cost %.2f -> %.2f ns", a, b)
		}
	}
}

// FuzzRobots asserts totality, the limit/grade rule and agreement with
// the reference matcher for the fuzzed file and a few paths.
func FuzzRobots(f *testing.F) {
	f.Add([]byte("User-agent: *\nDisallow: /a*b$\nAllow: /a\n"), "/axb")
	f.Add([]byte("User-agent: foobot\nUser-agent: bar\nDisallow: /%61\nSitemap: https://x/s.xml\n"), "/a")
	f.Fuzz(func(t *testing.T, src []byte, path string) {
		if len(path) > 64 {
			path = path[:64]
		}
		rb := ParseRobots(src, RobotsOptions{MaxItems: 50})
		limit := false
		for _, d := range rb.Diags {
			limit = limit || d.Code == RobotsLimitExceeded || d.Code == RobotsInputTooLarge
		}
		if limit != (rb.Grade == GradePartial) {
			t.Fatalf("limit %v grade %s", limit, rb.Grade)
		}
		// Rebuild the model from the parsed groups and compare decisions.
		var model []mgroup
		for _, g := range rb.Groups {
			mg := mgroup{agents: g.Agents}
			for _, ru := range g.Rules {
				if len(ru.Pattern) > 24 || strings.Count(ru.Pattern, "*") > 4 {
					return // keep the exponential reference cheap
				}
				mg.rules = append(mg.rules, mrule{allow: ru.Allow, pattern: ru.Pattern})
			}
			model = append(model, mg)
		}
		for _, ua := range []string{"foobot", "bar", "x"} {
			if got, want := rb.Allowed(ua, path), refAllowed(model, ua, path); got != want {
				t.Fatalf("Allowed(%q, %q) = %v, reference %v", ua, path, got, want)
			}
		}
	})
}

// FuzzSecurityTxt asserts totality, determinism, the limit/grade rule and
// that Valid implies at least one Contact and an unexpired Expires.
func FuzzSecurityTxt(f *testing.F) {
	f.Add([]byte("Contact: mailto:a@example.com\nExpires: 2030-01-01T00:00:00Z\n"), int64(0))
	f.Add([]byte("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\n- Contact: tel:+1\n-----BEGIN PGP SIGNATURE-----\n-----END PGP SIGNATURE-----\n"), int64(1e9))
	f.Fuzz(func(t *testing.T, src []byte, unix int64) {
		now := time.Unix(unix%(1<<34), 0).UTC()
		s := ParseSecurityTxt(src, now, SecurityOptions{MaxItems: 20})
		if fmt.Sprint(s) != fmt.Sprint(ParseSecurityTxt(src, now, SecurityOptions{MaxItems: 20})) {
			t.Fatal("not deterministic")
		}
		limit := false
		for _, d := range s.Diags {
			limit = limit || d.Code == SecurityLimitExceeded || d.Code == SecurityInputTooLarge
		}
		if limit != (s.Grade == GradePartial) || len(s.Fields) > 20 {
			t.Fatalf("limit %v grade %s", limit, s.Grade)
		}
		if s.Valid() && (len(s.Contact) == 0 || !s.HasExpires || !s.Expires.After(now)) {
			t.Fatalf("valid without contact or live expires")
		}
	})
}

// BenchmarkWK01_ParseRobots measures robots.txt parsing at n and 4n.
func BenchmarkWK01_ParseRobots(b *testing.B) {
	for _, n := range []int{32 << 10, 128 << 10} {
		rr := core.NewSplitMix64(7)
		var buf bytes.Buffer
		for buf.Len() < n {
			_, text := genRobots(rr)
			buf.Write(text)
		}
		src := buf.Bytes()
		b.Run(fmt.Sprintf("%dKiB", n>>10), func(b *testing.B) {
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			for b.Loop() {
				ParseRobots(src, RobotsOptions{})
			}
		})
	}
}

// BenchmarkWK02_Allowed measures one decision against a large file.
func BenchmarkWK02_Allowed(b *testing.B) {
	rr := core.NewSplitMix64(9)
	var buf bytes.Buffer
	for buf.Len() < 64<<10 {
		_, text := genRobots(rr)
		buf.Write(text)
	}
	rb := ParseRobots(buf.Bytes(), RobotsOptions{})
	b.ReportAllocs()
	for b.Loop() {
		rb.Allowed("foobot", "/a/b%61/x.gif?q=1")
	}
}

// BenchmarkWK03_ParseSecurityTxt measures security.txt parsing.
func BenchmarkWK03_ParseSecurityTxt(b *testing.B) {
	src := bytes.Repeat([]byte("Contact: mailto:a@example.com\nPolicy: https://example.com/p\n"), 500)
	src = append(src, "Expires: 2030-01-01T00:00:00Z\n"...)
	now := time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for b.Loop() {
		ParseSecurityTxt(src, now, SecurityOptions{})
	}
}
