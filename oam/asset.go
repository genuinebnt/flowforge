package oam

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"
)

// Asset type names (spec 5.2). Types without a Go struct in this package
// are carried as Opaque.
const (
	TypeFQDN             = "FQDN"
	TypeIPAddress        = "IPAddress"
	TypeNetblock         = "Netblock"
	TypeAutonomousSystem = "AutonomousSystem"
	TypeDomainRecord     = "DomainRecord"
	TypeIPNetRecord      = "IPNetRecord"
	TypeAutnumRecord     = "AutnumRecord"
	TypeURL              = "URL"
	TypeService          = "Service"
	TypeTLSCertificate   = "TLSCertificate"
	TypeFile             = "File"
	TypeOrganization     = "Organization"
	TypePerson           = "Person"
	TypeLocation         = "Location"
	TypePhone            = "Phone"
	TypeContactRecord    = "ContactRecord"
	TypeIdentifier       = "Identifier"
	TypeAccount          = "Account"
	TypeFundsTransfer    = "FundsTransfer"
	TypeProduct          = "Product"
	TypeProductRelease   = "ProductRelease"
)

// Asset is one OAM asset: the content of a graph entity.
//
// Concrete assets are value types. Their JSON form carries the "type"
// discriminator inside the object, as in spec 5.8; storage splits it off
// (SplitAsset).
type Asset interface {
	// AssetType returns the OAM type name, for example "FQDN".
	AssetType() string
	// Key returns the canonical de-duplication key of spec 5.4, for
	// example "fqdn:api.acme.com". Two spellings of the same asset yield
	// the same key. It fails with ErrInvalidAsset when the identity fields
	// are missing or invalid.
	Key() (string, error)
	// Normalize validates the asset and returns a copy whose identity
	// fields are in canonical form (spec 4.2), so that content merges
	// compare like with like. Normalize is idempotent and does not change
	// the key.
	Normalize() (Asset, error)
}

// modeledFold reports whether typ equals a modeled type name ignoring case.
func modeledFold(typ string) bool {
	for _, m := range []string{TypeFQDN, TypeIPAddress, TypeNetblock, TypeAutonomousSystem,
		TypeDomainRecord, TypeIPNetRecord, TypeAutnumRecord,
		TypeURL, TypeService, TypeTLSCertificate, TypeFile, TypeOrganization} {
		if strings.EqualFold(typ, m) {
			return true
		}
	}
	return false
}

// DecodeAsset decodes an asset object with its "type" discriminator. Known
// types decode into their struct; every other well-formed type name
// (including future OAM types) decodes into Opaque. DecodeAsset checks
// structure only; call Key or Normalize to validate the values.
func DecodeAsset(data []byte, mode DecodeMode) (Asset, error) {
	typ, err := peekType(data, ErrInvalidAsset)
	if err != nil {
		return nil, err
	}
	switch typ {
	case TypeFQDN:
		return decodeTyped[FQDN](data, mode)
	case TypeIPAddress:
		return decodeTyped[IPAddress](data, mode)
	case TypeNetblock:
		return decodeTyped[Netblock](data, mode)
	case TypeAutonomousSystem:
		return decodeTyped[AutonomousSystem](data, mode)
	case TypeDomainRecord:
		return decodeTyped[DomainRecord](data, mode)
	case TypeIPNetRecord:
		return decodeTyped[IPNetRecord](data, mode)
	case TypeAutnumRecord:
		return decodeTyped[AutnumRecord](data, mode)
	case TypeURL:
		return decodeTyped[URL](data, mode)
	case TypeService:
		return decodeTyped[Service](data, mode)
	case TypeTLSCertificate:
		return decodeTyped[TLSCertificate](data, mode)
	case TypeFile:
		return decodeTyped[File](data, mode)
	case TypeOrganization:
		return decodeTyped[Organization](data, mode)
	}
	var content map[string]any
	if err := decodeJSON(data, &content, mode); err != nil {
		return nil, fmt.Errorf("%s: %w", typ, err)
	}
	delete(content, "type")
	return Opaque{Type: typ, Content: content}, nil
}

func decodeTyped[T Asset](data []byte, mode DecodeMode) (Asset, error) {
	var v T
	if err := decodeJSON(data, &v, mode, "type"); err != nil {
		return nil, fmt.Errorf("%s: %w", v.AssetType(), err)
	}
	return v, nil
}

// EncodeAsset encodes a with its "type" discriminator.
func EncodeAsset(a Asset) ([]byte, error) {
	if a == nil {
		return nil, fmt.Errorf("%w: nil asset", ErrInvalidAsset)
	}
	b, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidAsset, err)
	}
	return b, nil
}

// SplitAsset separates an asset into the storage columns of spec 10.5:
// the type name and the content object (every field except "type"). The
// content holds only JSON-native values (numbers as json.Number).
func SplitAsset(a Asset) (assetType string, content map[string]any, err error) {
	b, err := EncodeAsset(a)
	if err != nil {
		return "", nil, err
	}
	m, err := objectOf(b)
	if err != nil {
		return "", nil, err
	}
	delete(m, "type")
	return a.AssetType(), m, nil
}

// JoinAsset is the inverse of SplitAsset. Stored data is normally joined
// with Lenient so fields added by newer writers are ignored.
func JoinAsset(assetType string, content map[string]any, mode DecodeMode) (Asset, error) {
	m := make(map[string]any, len(content)+1)
	maps.Copy(m, content)
	m["type"] = assetType
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidAsset, err)
	}
	return DecodeAsset(b, mode)
}

// prepareAsset normalizes a and returns its type, key and content.
func prepareAsset(a Asset) (n Asset, typ, key string, content map[string]any, err error) {
	if a == nil {
		return nil, "", "", nil, fmt.Errorf("%w: missing asset", ErrInvalidAsset)
	}
	if n, err = a.Normalize(); err != nil {
		return nil, "", "", nil, err
	}
	if key, err = n.Key(); err != nil {
		return nil, "", "", nil, err
	}
	typ, content, err = SplitAsset(n)
	if err != nil {
		return nil, "", "", nil, err
	}
	return n, typ, key, content, nil
}

func assetErr(typ, field string, err error) error {
	if err == nil {
		return fmt.Errorf("%w: %s.%s is required", ErrInvalidAsset, typ, field)
	}
	return fmt.Errorf("%w: %s.%s: %w", ErrInvalidAsset, typ, field, err)
}
