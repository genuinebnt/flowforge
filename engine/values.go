package engine

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/vm"
)

// IdempotencyKey returns hex(sha256(run_id || "/" || path)), the effect
// idempotency key of spec 9.5. run_id is the canonical UUID text.
func IdempotencyKey(run core.UUID, path string) string {
	sum := sha256.Sum256([]byte(run.String() + "/" + path))
	return hex.EncodeToString(sum[:])
}

// derivedID returns a deterministic UUID (version 8, RFC 9562 custom)
// from the parts. Ids of approvals, timers, decisions, snapshots,
// observations and outbox events are derived from the run and effect
// path, so a worker that re-performs a step after a crash produces the
// same ids and the repository upserts stay idempotent.
//
// PORT: the input is the parts joined by NUL bytes (no length prefixes,
// unlike oam.EntityUUID), SHA-256, first 16 bytes, then the version and
// variant bits. These ids are stored rows, so a Rust engine sharing the
// database must derive them identically.
func derivedID(parts ...string) core.UUID {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	var u core.UUID
	copy(u[:], h[:16])
	u[6] = (u[6] & 0x0f) | 0x80
	u[8] = (u[8] & 0x3f) | 0x80
	return u
}

func hexDecode(s string) ([]byte, error) { return hex.DecodeString(s) }

func hexHash(h [32]byte) string { return hex.EncodeToString(h[:]) }

// encodeResult returns the exact (base64 CBOR) and display (JSON) forms of
// an effect result value.
func encodeResult(m *bytecode.Module, v vm.Value) (string, json.RawMessage, error) {
	b, err := vm.EncodeValue(v)
	if err != nil {
		return "", nil, err
	}
	return base64.StdEncoding.EncodeToString(b), displayJSON(m, v), nil
}

// displayJSON renders v as JSON for the log: the JSON form of vm.ToJSON
// with proofs shown as their targets, or the display string for values
// JSON cannot carry (closures).
func displayJSON(m *bytecode.Module, v vm.Value) json.RawMessage {
	if b, err := vm.ToJSON(m, plainTargets(v, 0), vm.JSONOptions{UnwrapProofs: true}); err == nil {
		return b
	}
	b, _ := json.Marshal(vm.DisplayIn(m, v))
	return b
}

// displayArgs renders effect arguments as a JSON array.
func displayArgs(m *bytecode.Module, args []vm.Value) json.RawMessage {
	parts := make([]json.RawMessage, len(args))
	for i, a := range args {
		parts[i] = displayJSON(m, a)
	}
	b, err := json.Marshal(parts)
	if err != nil {
		return json.RawMessage("[]")
	}
	return b
}

// targetOf extracts the scope target text and kind of a VM value: a Host
// enum, a domain scalar, a String (kind inferred) or a ScopeProof (its
// target).
func targetOf(m *bytecode.Module, v vm.Value) (string, scope.TargetKind, bool) {
	switch v.Kind() {
	case vm.KindScopeProof:
		return targetOf(m, v.ProofTarget())
	case vm.KindFqdn:
		return v.Str(), scope.TargetHost, true
	case vm.KindIPAddress:
		return v.Str(), scope.TargetIP, true
	case vm.KindURL:
		return v.Str(), scope.TargetURL, true
	case vm.KindNetblock:
		return v.Str(), scope.TargetCIDR, true
	case vm.KindString:
		return v.Str(), scope.TargetUnspecified, true
	case vm.KindEnum:
		if v.TypeID() == bytecode.TypeHost && len(v.Fields()) == 1 {
			inner := v.Fields()[0]
			if v.Variant() == bytecode.VariantIP {
				return inner.Str(), scope.TargetIP, inner.Str() != ""
			}
			return inner.Str(), scope.TargetHost, inner.Str() != ""
		}
	}
	return "", 0, false
}

// normalizedTarget returns the normalized form of a target value, or "".
func normalizedTarget(m *bytecode.Module, v vm.Value) string {
	raw, kind, ok := targetOf(m, v)
	if !ok {
		return ""
	}
	t, err := scope.Normalize(raw, kind)
	if err != nil {
		return ""
	}
	return t.String()
}

// collectProofs appends every ScopeProof inside v (lists, maps, records,
// enums) to out.
func collectProofs(v vm.Value, out []vm.Value, depth int) []vm.Value {
	if depth > 64 {
		return out
	}
	switch v.Kind() {
	case vm.KindScopeProof:
		return append(out, v)
	case vm.KindList:
		for _, e := range v.Elems() {
			out = collectProofs(e, out, depth+1)
		}
	case vm.KindMap:
		for _, e := range v.MapKeys() {
			out = collectProofs(e, out, depth+1)
		}
		for _, e := range v.MapValues() {
			out = collectProofs(e, out, depth+1)
		}
	case vm.KindRecord, vm.KindEnum:
		for _, e := range v.Fields() {
			out = collectProofs(e, out, depth+1)
		}
	}
	return out
}

// enumName returns the variant name of an enum value in module m.
func enumName(m *bytecode.Module, v vm.Value) string {
	if v.Kind() != vm.KindEnum {
		return vm.DisplayIn(m, v)
	}
	l, ok := m.Layout(v.TypeID())
	if !ok || int(v.Variant()) >= len(l.Variants) {
		return strconv.Itoa(int(v.Variant()))
	}
	return strings.ToLower(l.Variants[v.Variant()].Name)
}

// effectMask maps a tool's declared effect class to verifier effect bits.
func effectMask(class string) bytecode.EffectMask {
	m, _ := bytecode.ParseEffect(class)
	return m
}

// activityClass maps a declared effect class to an activity class
// (ClassNone for effects that are not target-facing).
func activityClass(class string) core.ActivityClass {
	c, err := core.ParseActivityClass(class)
	if err != nil {
		return core.ClassNone
	}
	return c
}

// truncateUTF8 cuts s to at most n bytes on a rune boundary.
func truncateUTF8(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n], true
}

// plainTargets replaces ScopeProofs by their targets and Host values by
// the bare name or address, the form tools and humans expect (a Host's
// generic enum JSON is {"variant": "Name", "values": [...]}).
func plainTargets(v vm.Value, depth int) vm.Value {
	if depth > 64 {
		return v
	}
	switch v.Kind() {
	case vm.KindScopeProof:
		return plainTargets(v.ProofTarget(), depth+1)
	case vm.KindEnum:
		if v.TypeID() == bytecode.TypeHost && len(v.Fields()) == 1 {
			return vm.Str(v.Fields()[0].Str())
		}
		fs := v.Fields()
		if len(fs) == 0 {
			return v
		}
		out := make([]vm.Value, len(fs))
		for i, f := range fs {
			out[i] = plainTargets(f, depth+1)
		}
		return vm.Enum(v.TypeID(), v.Variant(), out...)
	case vm.KindRecord:
		fs := v.Fields()
		out := make([]vm.Value, len(fs))
		for i, f := range fs {
			out[i] = plainTargets(f, depth+1)
		}
		return vm.Record(v.TypeID(), out...)
	case vm.KindList:
		es := v.Elems()
		out := make([]vm.Value, len(es))
		for i, e := range es {
			out[i] = plainTargets(e, depth+1)
		}
		if v.IsStream() {
			return vm.Stream(out...)
		}
		return vm.List(out...)
	case vm.KindMap:
		ks, vs := v.MapKeys(), v.MapValues()
		nk, nv := make([]vm.Value, len(ks)), make([]vm.Value, len(vs))
		for i := range ks {
			nk[i], nv[i] = plainTargets(ks[i], depth+1), plainTargets(vs[i], depth+1)
		}
		if m, err := vm.Map(nk, nv); err == nil {
			return m
		}
	}
	return v
}

// DerivedID returns the deterministic id the engine derives from parts
// (a version 8 UUID of their SHA-256). Adapters use it for ids that must
// agree across processes.
func DerivedID(parts ...string) core.UUID { return derivedID(parts...) }
