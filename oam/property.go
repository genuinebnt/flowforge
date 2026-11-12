package oam

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"
)

// Property type names (spec 5.6).
const (
	PropertySource    = "SourceProperty"
	PropertySimple    = "SimpleProperty"
	PropertyDNSRecord = "DNSRecordProperty"
	PropertyVuln      = "VulnProperty"
	PropertyFlowforge = "FlowforgeProperty"
)

// Kinds of a FlowforgeProperty.
const (
	KindFact       = "fact"
	KindHypothesis = "hypothesis"
)

// Property is a tag attached to an entity or an edge. A target holds at
// most one property per (PropertyType, PropertyName).
type Property interface {
	// PropertyType returns the type name, for example "SourceProperty".
	PropertyType() string
	// PropertyName returns the name that identifies the tag on its target.
	PropertyName() string
	// Validate checks the property's values.
	Validate() error
}

// SourceProperty records that a data source reported the target, with
// the source's confidence in percent (0-100). One per source.
type SourceProperty struct {
	Name       string `json:"name"`
	Confidence int    `json:"confidence"`
}

// SimpleProperty is a generic string key-value pair.
type SimpleProperty struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// DNSRecordProperty holds DNS data that is not a graph edge (TXT, SOA).
type DNSRecordProperty struct {
	Name   string    `json:"property_name"`
	Header DNSHeader `json:"header"`
	Data   string    `json:"data"`
}

// VulnProperty references a vulnerability.
type VulnProperty struct {
	ID          string `json:"id"`
	Description string `json:"desc,omitempty"`
	Source      string `json:"source,omitempty"`
	Category    string `json:"category,omitempty"`
	Enum        string `json:"enum,omitempty"`
	Ref         string `json:"ref,omitempty"`
}

// FlowforgeProperty is a Flowforge note (agent notes, triage rank,
// hypotheses). Kind is required and is "fact" or "hypothesis".
type FlowforgeProperty struct {
	Name  string `json:"name"`
	Value any    `json:"value,omitempty"`
	Kind  string `json:"kind"`
}

// PropertyType implements Property.
func (SourceProperty) PropertyType() string { return PropertySource }

// PropertyType implements Property.
func (SimpleProperty) PropertyType() string { return PropertySimple }

// PropertyType implements Property.
func (DNSRecordProperty) PropertyType() string { return PropertyDNSRecord }

// PropertyType implements Property.
func (VulnProperty) PropertyType() string { return PropertyVuln }

// PropertyType implements Property.
func (FlowforgeProperty) PropertyType() string { return PropertyFlowforge }

// PropertyName implements Property.
func (p SourceProperty) PropertyName() string { return p.Name }

// PropertyName implements Property.
func (p SimpleProperty) PropertyName() string { return p.Name }

// PropertyName implements Property.
func (p DNSRecordProperty) PropertyName() string { return p.Name }

// PropertyName implements Property.
func (p VulnProperty) PropertyName() string { return p.ID }

// PropertyName implements Property.
func (p FlowforgeProperty) PropertyName() string { return p.Name }

// Validate implements Property.
func (p SourceProperty) Validate() error {
	if err := requireName(PropertySource, "name", p.Name); err != nil {
		return err
	}
	if p.Confidence < 0 || p.Confidence > 100 {
		return fmt.Errorf("%w: SourceProperty.confidence %d outside 0..100", ErrInvalidProperty, p.Confidence)
	}
	return nil
}

// Validate implements Property.
func (p SimpleProperty) Validate() error { return requireName(PropertySimple, "name", p.Name) }

// Validate implements Property.
func (p DNSRecordProperty) Validate() error {
	if err := requireName(PropertyDNSRecord, "property_name", p.Name); err != nil {
		return err
	}
	if err := p.Header.validate(); err != nil {
		return fmt.Errorf("%w: DNSRecordProperty.header: %w", ErrInvalidProperty, err)
	}
	return nil
}

// Validate implements Property.
func (p VulnProperty) Validate() error { return requireName(PropertyVuln, "id", p.ID) }

// Validate implements Property.
func (p FlowforgeProperty) Validate() error {
	if err := requireName(PropertyFlowforge, "name", p.Name); err != nil {
		return err
	}
	if p.Kind != KindFact && p.Kind != KindHypothesis {
		return fmt.Errorf("%w: FlowforgeProperty.kind must be %q or %q, got %q", ErrInvalidProperty, KindFact, KindHypothesis, p.Kind)
	}
	return nil
}

// MarshalJSON encodes the property with its type discriminator.
func (p SourceProperty) MarshalJSON() ([]byte, error) {
	type plain SourceProperty
	return marshalTagged(PropertySource, plain(p))
}

// MarshalJSON encodes the property with its type discriminator.
func (p SimpleProperty) MarshalJSON() ([]byte, error) {
	type plain SimpleProperty
	return marshalTagged(PropertySimple, plain(p))
}

// MarshalJSON encodes the property with its type discriminator.
func (p DNSRecordProperty) MarshalJSON() ([]byte, error) {
	type plain DNSRecordProperty
	return marshalTagged(PropertyDNSRecord, plain(p))
}

// MarshalJSON encodes the property with its type discriminator.
func (p VulnProperty) MarshalJSON() ([]byte, error) {
	type plain VulnProperty
	return marshalTagged(PropertyVuln, plain(p))
}

// MarshalJSON encodes the property with its type discriminator.
func (p FlowforgeProperty) MarshalJSON() ([]byte, error) {
	type plain FlowforgeProperty
	return marshalTagged(PropertyFlowforge, plain(p))
}

// DecodeProperty decodes a property object with its "type" discriminator.
// Strict rejects unknown keys (requests); Lenient ignores them (stored
// data), as spec 25.3 oam_007 requires.
func DecodeProperty(data []byte, mode DecodeMode) (Property, error) {
	typ, err := peekType(data, ErrInvalidProperty)
	if err != nil {
		return nil, err
	}
	switch typ {
	case PropertySource:
		return decodeProperty[SourceProperty](data, mode)
	case PropertySimple:
		return decodeProperty[SimpleProperty](data, mode)
	case PropertyDNSRecord:
		return decodeProperty[DNSRecordProperty](data, mode)
	case PropertyVuln:
		return decodeProperty[VulnProperty](data, mode)
	case PropertyFlowforge:
		return decodeProperty[FlowforgeProperty](data, mode)
	}
	return nil, fmt.Errorf("%w: %q", ErrUnknownPropertyType, typ)
}

func decodeProperty[T Property](data []byte, mode DecodeMode) (Property, error) {
	var p T
	if err := decodeJSON(data, &p, mode, "type"); err != nil {
		return nil, fmt.Errorf("%s: %w", p.PropertyType(), err)
	}
	return p, nil
}

// SplitProperty separates a property into the tag columns of spec 10.5:
// ptype, name and content (every field except "type").
func SplitProperty(p Property) (ptype, name string, content map[string]any, err error) {
	if p == nil {
		return "", "", nil, fmt.Errorf("%w: missing property", ErrInvalidProperty)
	}
	if err := p.Validate(); err != nil {
		return "", "", nil, err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", "", nil, fmt.Errorf("%w: %v", ErrInvalidProperty, err)
	}
	m, err := objectOf(b)
	if err != nil {
		return "", "", nil, err
	}
	delete(m, "type")
	return p.PropertyType(), p.PropertyName(), m, nil
}

// JoinProperty is the inverse of SplitProperty.
func JoinProperty(ptype string, content map[string]any, mode DecodeMode) (Property, error) {
	m := make(map[string]any, len(content)+1)
	maps.Copy(m, content)
	m["type"] = ptype
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidProperty, err)
	}
	return DecodeProperty(b, mode)
}

func requireName(typ, field, v string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("%w: %s.%s is required", ErrInvalidProperty, typ, field)
	}
	return nil
}
