package scope

import (
	"context"
	"fmt"
	"hash/fnv"
	"net/netip"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// Generators for property tests, the fuzz target and benchmarks. Values
// are drawn from small pools so generated rules and targets collide often
// enough to exercise matches, overlaps and deny precedence.

var (
	genLabels  = []string{"a", "b", "c", "acme", "com", "net", "api", "www", "xn--pple-43d", "_x"}
	genPaths   = []string{"/", "/a", "/a/", "/a/b", "/b/", "/a/b/", "/ab", "/a/b/c", "/b/x/y"}
	genSchemes = []string{"http", "https"}
	genPorts   = []string{"", ":80", ":443", ":8080"}
	genV4Bits  = []int{0, 8, 12, 14, 15, 16, 20, 24, 28, 30, 31, 32}
	genV6Bits  = []int{0, 16, 32, 40, 47, 48, 56, 64, 96, 127, 128}
	genZones   = []string{"UTC", "America/New_York", "Asia/Kolkata", "Europe/Berlin"}
	genDays    = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}
)

func pick[T any](r core.Rng, xs []T) T { return xs[core.IntN(r, len(xs))] }

func genName(r core.Rng) string {
	n := 1 + core.IntN(r, 4)
	labels := make([]string, n)
	for i := range labels {
		labels[i] = pick(r, genLabels)
	}
	return strings.Join(labels, ".")
}

func genV4(r core.Rng) netip.Addr {
	return netip.AddrFrom4([4]byte{10, byte(core.IntN(r, 4)), byte(core.IntN(r, 4)), byte(core.IntN(r, 256))})
}

func genV6(r core.Rng) netip.Addr {
	var b [16]byte
	b[0], b[1], b[2], b[3] = 0x20, 0x01, 0x0d, 0xb8
	b[5] = byte(core.IntN(r, 4))
	b[7] = byte(core.IntN(r, 4))
	b[15] = byte(core.IntN(r, 4))
	return netip.AddrFrom16(b)
}

func genAddr(r core.Rng) netip.Addr {
	if core.IntN(r, 3) == 0 {
		return genV6(r)
	}
	return genV4(r)
}

func genPrefix(r core.Rng) string {
	if core.IntN(r, 3) == 0 {
		return fmt.Sprintf("%s/%d", genV6(r), pick(r, genV6Bits))
	}
	return fmt.Sprintf("%s/%d", genV4(r), pick(r, genV4Bits))
}

func genHostText(r core.Rng) string {
	if core.IntN(r, 4) == 0 {
		a := genAddr(r)
		if a.Is6() {
			return "[" + a.String() + "]"
		}
		return a.String()
	}
	return genName(r)
}

func genURL(r core.Rng, withQuery bool) string {
	u := pick(r, genSchemes) + "://" + genHostText(r) + pick(r, genPorts) + pick(r, genPaths)
	if withQuery && core.IntN(r, 4) == 0 {
		u += "?q=1"
	}
	return u
}

func genRule(r core.Rng, id string) Rule {
	rule := Rule{ID: id, Effect: EffectAllow}
	if core.IntN(r, 3) == 0 {
		rule.Effect = EffectDeny
	}
	switch core.IntN(r, 11) {
	case 0, 1:
		rule.Kind, rule.Value = RuleDomain, genName(r)
	case 2, 3, 4:
		rule.Kind, rule.Value = RuleWildcardDomain, "*."+genName(r)
	case 5, 6, 7:
		rule.Kind, rule.Value = RuleCIDR, genPrefix(r)
	case 8, 9:
		rule.Kind, rule.Value = RuleURLPrefix, genURL(r, false)
	default:
		rule.Kind, rule.Value = RuleASN, fmt.Sprintf("AS%d", 64500+core.IntN(r, 10))
	}
	return rule
}

func genWindow(r core.Rng) Window {
	w := Window{TZ: pick(r, genZones)}
	for _, d := range genDays {
		if core.IntN(r, 2) == 0 {
			w.Days = append(w.Days, d)
		}
	}
	if len(w.Days) == 0 {
		w.Days = []string{pick(r, genDays)}
	}
	start := core.IntN(r, minutesPerDay)
	end := (start + 1 + core.IntN(r, minutesPerDay-1)) % minutesPerDay
	w.Start = fmt.Sprintf("%02d:%02d", start/60, start%60)
	w.End = fmt.Sprintf("%02d:%02d", end/60, end%60)
	return w
}

func genDocument(r core.Rng, maxRules int) Document {
	d := Document{Version: 1 + core.IntN(r, 9)}
	n := core.IntN(r, maxRules+1)
	for i := range n {
		d.Rules = append(d.Rules, genRule(r, fmt.Sprintf("r%d", i)))
	}
	d.Activity.MaxClass = pick(r, []core.ActivityClass{core.ClassPassive, core.ClassActive, core.ClassIntrusive, core.ClassIntrusive})
	for range core.IntN(r, 3) {
		d.Activity.Windows = append(d.Activity.Windows, genWindow(r))
	}
	return d
}

// genTarget returns a raw target and its kind, including noisy spellings
// (case, trailing dots, spaces, mapped IPv6) and plain garbage.
func genTarget(r core.Rng) (string, TargetKind) {
	switch core.IntN(r, 12) {
	case 0, 1, 2:
		s := genName(r)
		switch core.IntN(r, 4) {
		case 0:
			s = strings.ToUpper(s) + "."
		case 1:
			s = " " + s + " "
		}
		return s, pick(r, []TargetKind{TargetHost, TargetUnspecified})
	case 3, 4:
		a := genAddr(r)
		s := a.String()
		if a.Is4() && core.IntN(r, 3) == 0 {
			s = "::ffff:" + s
		}
		return s, pick(r, []TargetKind{TargetIP, TargetHost})
	case 5, 6, 7:
		return genURL(r, true), pick(r, []TargetKind{TargetURL, TargetUnspecified})
	case 8, 9:
		return genPrefix(r), pick(r, []TargetKind{TargetCIDR, TargetUnspecified})
	case 10:
		return genRule(r, "x").Value, TargetUnspecified
	default:
		return genGarbage(r), TargetKind(core.IntN(r, 6))
	}
}

func genGarbage(r core.Rng) string {
	const alphabet = "aA0.-_:/[]@%#?*\x00 аü．"
	runes := []rune(alphabet)
	n := core.IntN(r, 24)
	var b strings.Builder
	for range n {
		b.WriteRune(runes[core.IntN(r, len(runes))])
	}
	return b.String()
}

// genClass is mostly passive or active, sometimes intrusive, and rarely
// none or out of range.
func genClass(r core.Rng) core.ActivityClass {
	switch n := core.IntN(r, 20); {
	case n < 8:
		return core.ClassPassive
	case n < 15:
		return core.ClassActive
	case n < 18:
		return core.ClassIntrusive
	case n < 19:
		return core.ClassNone
	default:
		return core.ActivityClass(4 + core.IntN(r, 3))
	}
}

func genNow(r core.Rng) time.Time {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return base.Add(time.Duration(core.IntN(r, 365*24*60)) * time.Minute)
}

// hashResolver is a total, deterministic resolver for generated names:
// the answer depends only on the name, so two policies checked with it see
// the same addresses. About one name in eight fails.
type hashResolver struct{}

func (hashResolver) LookupAddrs(_ context.Context, host string) ([]netip.Addr, error) {
	h := fnv.New64a()
	h.Write([]byte(host))
	rng := core.NewSplitMix64(h.Sum64())
	if core.IntN(rng, 8) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchHost, host)
	}
	addrs := make([]netip.Addr, 1+core.IntN(rng, 3))
	for i := range addrs {
		addrs[i] = genAddr(rng)
	}
	return addrs, nil
}

func genRequest(r core.Rng) Request {
	target, kind := genTarget(r)
	return Request{Target: target, Kind: kind, Class: genClass(r), Now: genNow(r), KillSwitch: core.IntN(r, 50) == 0}
}
