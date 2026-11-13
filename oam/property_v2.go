package oam

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Property types added by Flowforge v2 (V2 design §3.3).
const (
	PropertyContent    = "ContentProperty"
	PropertyHTTPHeader = "HTTPHeaderProperty"
	PropertyParameter  = "ParameterProperty"
)

// Parameter locations of a ParameterProperty.
const (
	ParamQuery  = "query"
	ParamBody   = "body"
	ParamPath   = "path"
	ParamHeader = "header"
	ParamCookie = "cookie"
)

// TimeLayoutMillis is the RFC 3339 UTC millisecond layout used by v2
// property timestamps (V2 §1.6).
const TimeLayoutMillis = "2006-01-02T15:04:05.000Z"

// ContentProperty is the graph's pointer into the content store: the
// latest snapshot of an entity under one capture profile. One per
// (entity, profile); the tag name is the profile.
type ContentProperty struct {
	SnapshotID        string `json:"snapshot_id"`
	BodySHA256        string `json:"body_sha256"`
	Size              int64  `json:"size"`
	MIME              string `json:"mime,omitempty"`
	Profile           string `json:"profile"`
	NormalizerVersion string `json:"normalizer_version,omitempty"`
	LastChangedAt     string `json:"last_changed_at,omitempty"` // TimeLayoutMillis
	ChangeCount       int64  `json:"change_count"`
}

// HTTPHeaderProperty is one security-relevant response header (CSP, HSTS,
// CORS, X-Frame-Options, Server, cookie attribute summaries). The tag
// name is the lower-cased header name. Cookie values never enter the
// graph: a set-cookie value is a summary "name; Secure; HttpOnly;
// SameSite=Lax" in which only cookie attributes may carry "=".
// Request credentials (authorization, proxy-authorization, cookie) are
// rejected outright.
type HTTPHeaderProperty struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ParameterProperty is one parameter of an Endpoint. The tag name is
// "<location>:<name>", so the same name in two locations is two tags.
type ParameterProperty struct {
	Name     string `json:"name"`
	Location string `json:"location"`
	TypeHint string `json:"type_hint,omitempty"`
}

// PropertyType implements Property.
func (ContentProperty) PropertyType() string { return PropertyContent }

// PropertyType implements Property.
func (HTTPHeaderProperty) PropertyType() string { return PropertyHTTPHeader }

// PropertyType implements Property.
func (ParameterProperty) PropertyType() string { return PropertyParameter }

// PropertyName implements Property.
func (p ContentProperty) PropertyName() string { return p.Profile }

// PropertyName implements Property.
func (p HTTPHeaderProperty) PropertyName() string { return p.Name }

// PropertyName implements Property.
func (p ParameterProperty) PropertyName() string { return p.Location + ":" + p.Name }

// ValidProfile reports whether s is a capture-profile name:
// [A-Za-z0-9][A-Za-z0-9._:-]{0,63}.
func ValidProfile(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !alnum && (i == 0 || strings.IndexByte("._:-", c) < 0) {
			return false
		}
	}
	return true
}

func propErr(typ, field, format string, args ...any) error {
	return fmt.Errorf("%w: %s.%s: "+format, append([]any{ErrInvalidProperty, typ, field}, args...)...)
}

// Validate implements Property.
func (p ContentProperty) Validate() error {
	const t = PropertyContent
	switch {
	case !ValidProfile(p.Profile):
		return propErr(t, "profile", "bad profile %q", p.Profile)
	case strings.TrimSpace(p.SnapshotID) == "" || len(p.SnapshotID) > 256 || hasControl(p.SnapshotID):
		return propErr(t, "snapshot_id", "required, at most 256 printable bytes")
	case len(p.BodySHA256) != 64 || !isHex(p.BodySHA256) || strings.ToLower(p.BodySHA256) != p.BodySHA256:
		return propErr(t, "body_sha256", "must be 64 lower-case hex digits")
	case p.Size < 0:
		return propErr(t, "size", "negative")
	case p.ChangeCount < 0:
		return propErr(t, "change_count", "negative")
	}
	if p.MIME != "" && !validMIME(p.MIME) {
		return propErr(t, "mime", "%q is not a lower-case type/subtype", p.MIME)
	}
	if p.NormalizerVersion != "" && !validNormalizerVersion(p.NormalizerVersion) {
		return propErr(t, "normalizer_version", "%q is not <name>@<n>", p.NormalizerVersion)
	}
	if p.LastChangedAt != "" {
		if _, err := time.Parse(TimeLayoutMillis, p.LastChangedAt); err != nil {
			return propErr(t, "last_changed_at", "%q is not RFC 3339 UTC with milliseconds", p.LastChangedAt)
		}
	}
	return nil
}

// sensitiveRequestHeaders never enter the graph.
var sensitiveRequestHeaders = []string{"authorization", "cookie", "proxy-authorization"}

// cookieAttributes may carry "=" in a set-cookie summary.
var cookieAttributes = []string{"domain", "expires", "max-age", "path", "priority", "samesite"}

// Validate implements Property.
func (p HTTPHeaderProperty) Validate() error {
	const t = PropertyHTTPHeader
	if p.Name == "" || len(p.Name) > 128 || strings.ToLower(p.Name) != p.Name {
		return propErr(t, "name", "must be a lower-case header name")
	}
	for i := 0; i < len(p.Name); i++ {
		if !isTChar(p.Name[i]) {
			return propErr(t, "name", "%q is not an HTTP token", p.Name)
		}
	}
	if slices.Contains(sensitiveRequestHeaders, p.Name) {
		return propErr(t, "name", "%s is a credential header and is never stored", p.Name)
	}
	if len(p.Value) > 8192 || hasControl(p.Value) {
		return propErr(t, "value", "at most 8192 bytes without control characters")
	}
	if p.Name == "set-cookie" {
		for _, item := range strings.FieldsFunc(p.Value, func(r rune) bool { return r == ';' || r == ',' }) {
			k, _, hasEq := strings.Cut(strings.TrimSpace(item), "=")
			if hasEq && !slices.Contains(cookieAttributes, strings.ToLower(strings.TrimSpace(k))) {
				return propErr(t, "value", "set-cookie summary carries a cookie value (%q=...)", strings.TrimSpace(k))
			}
		}
	}
	return nil
}

// ParameterLocations lists the ParameterProperty locations, sorted.
func ParameterLocations() []string {
	return []string{ParamBody, ParamCookie, ParamHeader, ParamPath, ParamQuery}
}

// Validate implements Property.
func (p ParameterProperty) Validate() error {
	const t = PropertyParameter
	switch {
	case strings.TrimSpace(p.Name) == "" || len(p.Name) > 256 || hasControl(p.Name):
		return propErr(t, "name", "required, at most 256 printable bytes")
	case !slices.Contains(ParameterLocations(), p.Location):
		return propErr(t, "location", "%q is not one of %v", p.Location, ParameterLocations())
	case p.TypeHint != "" && (len(p.TypeHint) > 32 || !isLowerIdent(p.TypeHint, true)):
		return propErr(t, "type_hint", "%q is not a lower-case identifier", p.TypeHint)
	}
	return nil
}

// MarshalJSON encodes the property with its type discriminator.
func (p ContentProperty) MarshalJSON() ([]byte, error) {
	type plain ContentProperty
	return marshalTagged(PropertyContent, plain(p))
}

// MarshalJSON encodes the property with its type discriminator.
func (p HTTPHeaderProperty) MarshalJSON() ([]byte, error) {
	type plain HTTPHeaderProperty
	return marshalTagged(PropertyHTTPHeader, plain(p))
}

// MarshalJSON encodes the property with its type discriminator.
func (p ParameterProperty) MarshalJSON() ([]byte, error) {
	type plain ParameterProperty
	return marshalTagged(PropertyParameter, plain(p))
}

func hasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

func validMIME(s string) bool {
	typ, sub, ok := strings.Cut(s, "/")
	if !ok || typ == "" || sub == "" || strings.ToLower(s) != s || len(s) > 128 {
		return false
	}
	for _, part := range []string{typ, sub} {
		for i := 0; i < len(part); i++ {
			if !isTChar(part[i]) {
				return false
			}
		}
	}
	return true
}

func validNormalizerVersion(s string) bool {
	name, n, ok := strings.Cut(s, "@")
	if !ok || name == "" || !isDigits(n) || len(s) > 128 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}
