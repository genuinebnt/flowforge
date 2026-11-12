package oam

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/genuinebnt/flowforge/core"
)

// Opaque carries every asset type this package does not model: the v0.1
// unmodeled OAM types (Person, Location, Phone, ContactRecord, Identifier,
// Account, FundsTransfer, Product, ProductRelease) and any future type, so
// imports never fail (spec 5.2).
//
// Key: <lowercased type>:<sha256 of the canonical JSON of Content>. Product
// deliberately has no name-based key in v0.1 (see OPEN_QUESTIONS.md).
type Opaque struct {
	Type    string
	Content map[string]any // every field except "type"
}

// AssetType implements Asset.
func (a Opaque) AssetType() string { return a.Type }

// Key implements Asset.
func (a Opaque) Key() (string, error) {
	n, err := a.normalized()
	if err != nil {
		return "", err
	}
	b, err := core.CanonicalJSON(n.Content)
	if err != nil {
		return "", fmt.Errorf("%w: %s content: %v", ErrInvalidAsset, a.Type, err)
	}
	return strings.ToLower(n.Type) + ":" + core.SHA256Hex(b), nil
}

// Normalize implements Asset: Content is deep-copied into JSON-native
// values. A modeled type name (in any letter case), a bad type name or a "type" key inside
// Content is an error.
func (a Opaque) Normalize() (Asset, error) { return a.normalized() }

func (a Opaque) normalized() (Opaque, error) {
	if !validTypeName(a.Type) {
		return Opaque{}, fmt.Errorf("%w: bad type name %q", ErrInvalidAsset, a.Type)
	}
	if modeledFold(a.Type) {
		return Opaque{}, fmt.Errorf("%w: %s is (a case variant of) a modeled type and cannot be opaque", ErrInvalidAsset, a.Type)
	}
	if _, ok := a.Content["type"]; ok {
		return Opaque{}, fmt.Errorf("%w: %s content must not contain \"type\"", ErrInvalidAsset, a.Type)
	}
	content := map[string]any{}
	if len(a.Content) > 0 {
		v, err := normalizeJSON(a.Content)
		if err != nil {
			return Opaque{}, fmt.Errorf("%w: %s content: %w", ErrInvalidAsset, a.Type, err)
		}
		content, _ = v.(map[string]any)
	}
	return Opaque{Type: a.Type, Content: content}, nil
}

// MarshalJSON encodes the content with the type discriminator added.
func (a Opaque) MarshalJSON() ([]byte, error) {
	m := make(map[string]any, len(a.Content)+1)
	maps.Copy(m, a.Content)
	m["type"] = a.Type
	return json.Marshal(m)
}
