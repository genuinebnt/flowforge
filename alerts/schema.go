package alerts

import (
	"encoding/json"
	"math"
	"sort"
	"time"
)

// getter reads one field of an event. It returns nullValue when the
// section or field is absent.
type getter func(ev *Event) value

// fieldSpec is one addressable field of the event schema.
type fieldSpec struct {
	name string
	typ  typ
	get  getter
}

// rootVariables lists the variables in scope in a rule (spec 15.2).
func rootVariables() []string {
	return []string{"approval", "change", "edge", "entity", "event", "run"}
}

// schemaFields returns the fields of a root variable, or nil if root is
// not a variable. The schema is the contract between rule authors and the
// event producers:
//
//   - A missing section (no Entity on a run event, say) makes every field
//     of that root absent. Present sections expose their string and number
//     fields as-is; zero times count as absent.
//   - entity exposes the Flowforge columns plus the OAM content fields of
//     every asset type modelled in spec 5.4, flattened. Fixed columns
//     shadow content fields of the same name (DomainRecord.id is not
//     reachable). A content field is absent when the asset does not carry
//     it or carries it with a different JSON type.
func schemaFields(root string) []fieldSpec {
	switch root {
	case "event":
		return []fieldSpec{
			{"id", tStringT, func(ev *Event) value { return stringVal(ev.ID) }},
			{"type", tStringT, func(ev *Event) value { return stringVal(ev.Type) }},
			{"schema_version", tIntT, func(ev *Event) value { return intVal(ev.SchemaVersion) }},
			{"tenant", tStringT, func(ev *Event) value { return stringVal(ev.TenantID) }},
			{"program", tStringT, func(ev *Event) value { return stringVal(ev.ProgramID) }},
			{"occurred_at", tTimeT, func(ev *Event) value { return timeOrNull(ev.OccurredAt) }},
		}
	case "change":
		c := func(f func(*Change) value) getter {
			return func(ev *Event) value {
				if ev.Change == nil {
					return nullValue
				}
				return f(ev.Change)
			}
		}
		return []fieldSpec{
			{"id", tStringT, c(func(x *Change) value { return stringVal(x.ID) })},
			{"seq", tIntT, c(func(x *Change) value { return intVal(x.Seq) })},
			{"type", tStringT, c(func(x *Change) value { return stringVal(x.Type) })},
			{"entity_id", tStringT, c(func(x *Change) value { return stringVal(x.EntityID) })},
			{"edge_id", tStringT, c(func(x *Change) value { return stringVal(x.EdgeID) })},
			{"run_id", tStringT, c(func(x *Change) value { return stringVal(x.RunID) })},
			{"patch", tObjectT, c(func(x *Change) value { return objectVal(x.Patch) })},
			{"at", tTimeT, c(func(x *Change) value { return timeOrNull(x.At) })},
		}
	case "entity":
		return entityFields()
	case "edge":
		e := func(f func(*Edge) value) getter {
			return func(ev *Event) value {
				if ev.Edge == nil {
					return nullValue
				}
				return f(ev.Edge)
			}
		}
		return []fieldSpec{
			{"id", tStringT, e(func(x *Edge) value { return stringVal(x.ID) })},
			{"relation_type", tStringT, e(func(x *Edge) value { return stringVal(x.RelationType) })},
			{"label", tStringT, e(func(x *Edge) value { return stringVal(x.Label) })},
			{"discriminator", tStringT, e(func(x *Edge) value { return stringVal(x.Discriminator) })},
			{"from_entity_id", tStringT, e(func(x *Edge) value { return stringVal(x.FromEntityID) })},
			{"to_entity_id", tStringT, e(func(x *Edge) value { return stringVal(x.ToEntityID) })},
			{"confidence", tFloatT, e(func(x *Edge) value { return floatVal(x.Confidence) })},
			{"state", tStringT, e(func(x *Edge) value { return stringVal(x.State) })},
			{"created_at", tTimeT, e(func(x *Edge) value { return timeOrNull(x.CreatedAt) })},
			{"last_seen", tTimeT, e(func(x *Edge) value { return timeOrNull(x.LastSeen) })},
		}
	case "run":
		r := func(f func(*Run) value) getter {
			return func(ev *Event) value {
				if ev.Run == nil {
					return nullValue
				}
				return f(ev.Run)
			}
		}
		return []fieldSpec{
			{"id", tStringT, r(func(x *Run) value { return stringVal(x.ID) })},
			{"seq", tIntT, r(func(x *Run) value { return intVal(x.Seq) })},
			{"type", tStringT, r(func(x *Run) value { return stringVal(x.Type) })},
			{"workflow_name", tStringT, r(func(x *Run) value { return stringVal(x.WorkflowName) })},
			{"workflow_version", tIntT, r(func(x *Run) value { return intVal(x.WorkflowVersion) })},
			{"status", tStringT, r(func(x *Run) value { return stringVal(x.Status) })},
			{"code", tStringT, r(func(x *Run) value { return stringVal(x.Code) })},
			{"message", tStringT, r(func(x *Run) value { return stringVal(x.Message) })},
			{"path", tStringT, r(func(x *Run) value { return stringVal(x.Path) })},
			{"resource", tStringT, r(func(x *Run) value { return stringVal(x.Resource) })},
			{"payload", tObjectT, r(func(x *Run) value { return objectVal(x.Payload) })},
		}
	case "approval":
		a := func(f func(*Approval) value) getter {
			return func(ev *Event) value {
				if ev.Approval == nil {
					return nullValue
				}
				return f(ev.Approval)
			}
		}
		return []fieldSpec{
			{"id", tStringT, a(func(x *Approval) value { return stringVal(x.ID) })},
			{"run_id", tStringT, a(func(x *Approval) value { return stringVal(x.RunID) })},
			{"role", tStringT, a(func(x *Approval) value { return stringVal(x.Role) })},
			{"status", tStringT, a(func(x *Approval) value { return stringVal(x.Status) })},
			{"reason", tStringT, a(func(x *Approval) value { return stringVal(x.Reason) })},
			{"effect_path", tStringT, a(func(x *Approval) value { return stringVal(x.EffectPath) })},
			{"expires_at", tTimeT, a(func(x *Approval) value { return timeOrNull(x.ExpiresAt) })},
		}
	}
	return nil
}

// contentFieldKinds lists the OAM content fields of spec 5.4 by type.
// Fields whose JSON type differs between assets are typed by their most
// common use (version, type and the record dates are strings in OAM; the
// dates are parsed as RFC 3339 so they can be compared with times).
func contentFieldKinds() map[string]typ {
	m := map[string]typ{}
	for _, n := range []string{
		"name", "address", "ip_version", "cidr", "url", "scheme", "username", "password", "host",
		"path", "options", "fragment", "unique_id", "service_type", "output", "version",
		"serial_number", "subject_common_name", "issuer_common_name", "signature_algorithm",
		"public_key_algorithm", "subject_key_id", "authority_key_id", "type", "raw", "domain",
		"punycode", "extension", "whois_server", "handle", "start_address", "end_address",
		"method", "country", "parent_handle", "legal_name", "jurisdiction", "registration_id", "industry",
	} {
		m[n] = tStringT
	}
	for _, n := range []string{"number", "port", "output_length", "headcount"} {
		m[n] = tIntT
	}
	for _, n := range []string{"is_ca", "dnssec", "active", "non_profit"} {
		m[n] = tBoolT
	}
	for _, n := range []string{"not_before", "not_after", "created_date", "updated_date", "expiration_date", "founding_date"} {
		m[n] = tTimeT
	}
	for _, n := range []string{"key_usage", "crl_distribution_points", "status", "target_markets"} {
		m[n] = tStrList
	}
	m["attributes"] = tObjectT
	return m
}

func entityFields() []fieldSpec {
	e := func(f func(*Entity) value) getter {
		return func(ev *Event) value {
			if ev.Entity == nil {
				return nullValue
			}
			return f(ev.Entity)
		}
	}
	fields := []fieldSpec{
		{"id", tStringT, e(func(x *Entity) value { return stringVal(x.ID) })},
		{"asset_type", tStringT, e(func(x *Entity) value { return stringVal(x.AssetType) })},
		{"key", tStringT, e(func(x *Entity) value { return stringVal(x.Key) })},
		{"scope_status", tStringT, e(func(x *Entity) value { return stringVal(x.ScopeStatus) })},
		{"confidence", tFloatT, e(func(x *Entity) value { return floatVal(x.Confidence) })},
		{"state", tStringT, e(func(x *Entity) value { return stringVal(x.State) })},
		{"created_at", tTimeT, e(func(x *Entity) value { return timeOrNull(x.CreatedAt) })},
		{"last_seen", tTimeT, e(func(x *Entity) value { return timeOrNull(x.LastSeen) })},
		{"first_seen_run", tStringT, e(func(x *Entity) value { return stringVal(x.FirstSeenRun) })},
		{"last_seen_run", tStringT, e(func(x *Entity) value { return stringVal(x.LastSeenRun) })},
	}
	fixed := map[string]bool{}
	for _, f := range fields {
		fixed[f.name] = true
	}
	content := contentFieldKinds()
	names := make([]string, 0, len(content))
	for n := range content {
		if !fixed[n] {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		name, t := n, content[n]
		fields = append(fields, fieldSpec{name, t, e(func(x *Entity) value { return contentValue(x.Content[name], t) })})
	}
	return fields
}

// lookupField resolves root.field.
func lookupField(root, field string) (fieldSpec, bool) {
	for _, f := range schemaFields(root) {
		if f.name == field {
			return f, true
		}
	}
	return fieldSpec{}, false
}

func timeOrNull(t time.Time) value {
	if t.IsZero() {
		return nullValue
	}
	return timeVal(t)
}

// contentValue converts a decoded JSON content value to the declared
// type, or null when the JSON type does not fit.
func contentValue(raw any, t typ) value {
	if raw == nil {
		return nullValue
	}
	switch t.k {
	case kString:
		if s, ok := raw.(string); ok {
			return stringVal(s)
		}
	case kInt:
		if i, ok := toInt(raw); ok {
			return intVal(i)
		}
	case kBool:
		if b, ok := raw.(bool); ok {
			return boolVal(b)
		}
	case kTime:
		switch x := raw.(type) {
		case time.Time:
			return timeOrNull(x)
		case string:
			if ts, err := time.Parse(time.RFC3339Nano, x); err == nil {
				return timeVal(ts)
			}
		}
	case kList:
		switch x := raw.(type) {
		case []string:
			out := make([]value, len(x))
			for i, s := range x {
				out[i] = stringVal(s)
			}
			return value{k: kList, list: out}
		case []any:
			out := make([]value, 0, len(x))
			for _, e := range x {
				if s, ok := e.(string); ok {
					out = append(out, stringVal(s))
				}
			}
			return value{k: kList, list: out}
		}
	case kObject:
		if m, ok := raw.(map[string]any); ok {
			return objectVal(m)
		}
	}
	return nullValue
}

func toInt(raw any) (int64, bool) {
	switch x := raw.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	case int32:
		return int64(x), true
	case float64:
		if x == math.Trunc(x) && x >= -(1<<63) && x < 1<<63 {
			return int64(x), true
		}
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, true
		}
	}
	return 0, false
}
