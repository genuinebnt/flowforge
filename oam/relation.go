package oam

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Relation type names (spec 5.5).
const (
	RelationSimple   = "SimpleRelation"
	RelationPort     = "PortRelation"
	RelationBasicDNS = "BasicDNSRelation"
	RelationPrefDNS  = "PrefDNSRelation"
	RelationSRVDNS   = "SRVDNSRelation"
)

// Relation is the typed content of an edge. The relation type fixes the
// JSON shape; the label carries the meaning.
type Relation interface {
	// RelationType returns the OAM relation type, for example "PortRelation".
	RelationType() string
	// RelationLabel returns the label, for example "port".
	RelationLabel() string
	// Discriminator returns the part of the edge identity beyond the
	// label (spec 5.7): "tcp/443" for port relations, "rr<rr_type>" for
	// DNS relations and "" otherwise.
	Discriminator() string
	// Normalize validates the relation and returns a copy with the label
	// and protocol lowercased.
	Normalize() (Relation, error)
}

// DNSHeader is the resource-record header of a DNS relation.
type DNSHeader struct {
	RRType int `json:"rr_type"`
	Class  int `json:"class"`
	TTL    int `json:"ttl"`
}

func (h DNSHeader) validate() error {
	switch {
	case h.RRType < 1 || h.RRType > 65535:
		return fmt.Errorf("%w: rr_type %d outside 1..65535", ErrInvalidRelation, h.RRType)
	case h.Class < 0 || h.Class > 65535:
		return fmt.Errorf("%w: class %d outside 0..65535", ErrInvalidRelation, h.Class)
	case h.TTL < 0 || h.TTL > 1<<31-1:
		return fmt.Errorf("%w: ttl %d outside 0..2^31-1", ErrInvalidRelation, h.TTL)
	}
	return nil
}

func (h DNSHeader) discriminator() string { return "rr" + strconv.Itoa(h.RRType) }

// SimpleRelation is a generic link carrying a label. Status (ff, V2
// §3.2) is the HTTP status of a redirects_to hop (300-399) and is allowed
// on no other label.
type SimpleRelation struct {
	Label  string `json:"label"`
	Status int    `json:"status,omitempty"`
}

// PortRelation binds a host or URL to a service port.
type PortRelation struct {
	Label      string `json:"label"`
	PortNumber int    `json:"port_number"`
	Protocol   string `json:"protocol"`
}

// BasicDNSRelation is an A, AAAA, CNAME, NS or PTR record.
type BasicDNSRelation struct {
	Label  string    `json:"label"`
	Header DNSHeader `json:"header"`
}

// PrefDNSRelation is an MX record.
type PrefDNSRelation struct {
	Label      string    `json:"label"`
	Header     DNSHeader `json:"header"`
	Preference int       `json:"preference"`
}

// SRVDNSRelation is an SRV record.
type SRVDNSRelation struct {
	Label    string    `json:"label"`
	Header   DNSHeader `json:"header"`
	Priority int       `json:"priority"`
	Weight   int       `json:"weight"`
	Port     int       `json:"port"`
}

// RelationType implements Relation.
func (SimpleRelation) RelationType() string { return RelationSimple }

// RelationType implements Relation.
func (PortRelation) RelationType() string { return RelationPort }

// RelationType implements Relation.
func (BasicDNSRelation) RelationType() string { return RelationBasicDNS }

// RelationType implements Relation.
func (PrefDNSRelation) RelationType() string { return RelationPrefDNS }

// RelationType implements Relation.
func (SRVDNSRelation) RelationType() string { return RelationSRVDNS }

// RelationLabel implements Relation.
func (r SimpleRelation) RelationLabel() string { return r.Label }

// RelationLabel implements Relation.
func (r PortRelation) RelationLabel() string { return r.Label }

// RelationLabel implements Relation.
func (r BasicDNSRelation) RelationLabel() string { return r.Label }

// RelationLabel implements Relation.
func (r PrefDNSRelation) RelationLabel() string { return r.Label }

// RelationLabel implements Relation.
func (r SRVDNSRelation) RelationLabel() string { return r.Label }

// Discriminator implements Relation.
func (SimpleRelation) Discriminator() string { return "" }

// Discriminator implements Relation.
func (r PortRelation) Discriminator() string {
	return strings.ToLower(r.Protocol) + "/" + strconv.Itoa(r.PortNumber)
}

// Discriminator implements Relation.
func (r BasicDNSRelation) Discriminator() string { return r.Header.discriminator() }

// Discriminator implements Relation.
func (r PrefDNSRelation) Discriminator() string { return r.Header.discriminator() }

// Discriminator implements Relation.
func (r SRVDNSRelation) Discriminator() string { return r.Header.discriminator() }

// Normalize implements Relation.
func (r SimpleRelation) Normalize() (Relation, error) {
	var err error
	if r.Label, err = normalizeLabel(r.Label); err != nil {
		return nil, err
	}
	if r.Status != 0 {
		if r.Label != LabelRedirectsTo {
			return nil, fmt.Errorf("%w: status is only defined for %s, not %q", ErrInvalidRelation, LabelRedirectsTo, r.Label)
		}
		if r.Status < 300 || r.Status > 399 {
			return nil, fmt.Errorf("%w: redirect status %d outside 300..399", ErrInvalidRelation, r.Status)
		}
	}
	return r, nil
}

// Normalize implements Relation.
func (r PortRelation) Normalize() (Relation, error) {
	var err error
	if r.Label, err = normalizeLabel(r.Label); err != nil {
		return nil, err
	}
	if r.PortNumber < 1 || r.PortNumber > 65535 {
		return nil, fmt.Errorf("%w: port_number %d outside 1..65535", ErrInvalidRelation, r.PortNumber)
	}
	r.Protocol = strings.ToLower(strings.TrimSpace(r.Protocol))
	if !isLowerIdent(r.Protocol, false) {
		return nil, fmt.Errorf("%w: bad protocol %q", ErrInvalidRelation, r.Protocol)
	}
	return r, nil
}

// Normalize implements Relation.
func (r BasicDNSRelation) Normalize() (Relation, error) {
	var err error
	if r.Label, err = normalizeLabel(r.Label); err != nil {
		return nil, err
	}
	return r, r.Header.validate()
}

// Normalize implements Relation.
func (r PrefDNSRelation) Normalize() (Relation, error) {
	var err error
	if r.Label, err = normalizeLabel(r.Label); err != nil {
		return nil, err
	}
	if r.Preference < 0 || r.Preference > 65535 {
		return nil, fmt.Errorf("%w: preference %d outside 0..65535", ErrInvalidRelation, r.Preference)
	}
	return r, r.Header.validate()
}

// Normalize implements Relation.
func (r SRVDNSRelation) Normalize() (Relation, error) {
	var err error
	if r.Label, err = normalizeLabel(r.Label); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		name string
		v    int
	}{{"priority", r.Priority}, {"weight", r.Weight}, {"port", r.Port}} {
		if f.v < 0 || f.v > 65535 {
			return nil, fmt.Errorf("%w: %s %d outside 0..65535", ErrInvalidRelation, f.name, f.v)
		}
	}
	return r, r.Header.validate()
}

// MarshalJSON encodes the relation with its type discriminator.
func (r SimpleRelation) MarshalJSON() ([]byte, error) {
	type plain SimpleRelation
	return marshalTagged(RelationSimple, plain(r))
}

// MarshalJSON encodes the relation with its type discriminator.
func (r PortRelation) MarshalJSON() ([]byte, error) {
	type plain PortRelation
	return marshalTagged(RelationPort, plain(r))
}

// MarshalJSON encodes the relation with its type discriminator.
func (r BasicDNSRelation) MarshalJSON() ([]byte, error) {
	type plain BasicDNSRelation
	return marshalTagged(RelationBasicDNS, plain(r))
}

// MarshalJSON encodes the relation with its type discriminator.
func (r PrefDNSRelation) MarshalJSON() ([]byte, error) {
	type plain PrefDNSRelation
	return marshalTagged(RelationPrefDNS, plain(r))
}

// MarshalJSON encodes the relation with its type discriminator.
func (r SRVDNSRelation) MarshalJSON() ([]byte, error) {
	type plain SRVDNSRelation
	return marshalTagged(RelationSRVDNS, plain(r))
}

// DecodeRelation decodes a relation object with its "type" discriminator.
func DecodeRelation(data []byte, mode DecodeMode) (Relation, error) {
	typ, err := peekType(data, ErrInvalidRelation)
	if err != nil {
		return nil, err
	}
	switch typ {
	case RelationSimple:
		return decodeRelation[SimpleRelation](data, mode)
	case RelationPort:
		return decodeRelation[PortRelation](data, mode)
	case RelationBasicDNS:
		return decodeRelation[BasicDNSRelation](data, mode)
	case RelationPrefDNS:
		return decodeRelation[PrefDNSRelation](data, mode)
	case RelationSRVDNS:
		return decodeRelation[SRVDNSRelation](data, mode)
	}
	return nil, fmt.Errorf("%w: %q", ErrUnknownRelationType, typ)
}

func decodeRelation[T Relation](data []byte, mode DecodeMode) (Relation, error) {
	var r T
	if err := decodeJSON(data, &r, mode, "type"); err != nil {
		return nil, fmt.Errorf("%s: %w", r.RelationType(), err)
	}
	return r, nil
}

// SplitRelation separates a relation into the edge columns of spec 10.5:
// relation type, label, discriminator and content (the fields other than
// type and label). The relation is normalized first.
func SplitRelation(r Relation) (relType, label, discriminator string, content map[string]any, err error) {
	if r == nil {
		return "", "", "", nil, fmt.Errorf("%w: missing relation", ErrInvalidRelation)
	}
	n, err := r.Normalize()
	if err != nil {
		return "", "", "", nil, err
	}
	b, err := json.Marshal(n)
	if err != nil {
		return "", "", "", nil, fmt.Errorf("%w: %v", ErrInvalidRelation, err)
	}
	m, err := objectOf(b)
	if err != nil {
		return "", "", "", nil, err
	}
	delete(m, "type")
	delete(m, "label")
	return n.RelationType(), n.RelationLabel(), n.Discriminator(), m, nil
}

// JoinRelation is the inverse of SplitRelation.
func JoinRelation(relType, label string, content map[string]any, mode DecodeMode) (Relation, error) {
	m := make(map[string]any, len(content)+2)
	for k, v := range content {
		m[k] = v
	}
	m["type"], m["label"] = relType, label
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRelation, err)
	}
	return DecodeRelation(b, mode)
}

func normalizeLabel(label string) (string, error) {
	l := strings.ToLower(strings.TrimSpace(label))
	if !isLowerIdent(l, true) || len(l) > 64 {
		return "", fmt.Errorf("%w: bad label %q", ErrInvalidRelation, label)
	}
	return l, nil
}

// isLowerIdent accepts [a-z][a-z0-9_]* (underscores only when allowed).
func isLowerIdent(s string, underscore bool) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9' && i > 0:
		case c == '_' && underscore && i > 0:
		default:
			return false
		}
	}
	return true
}
