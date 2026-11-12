package oam

import (
	"strconv"
	"strings"

	"github.com/genuinebnt/flowforge/core/netnorm"
)

// DomainRecord is WHOIS/RDAP registration data for a domain.
// Key: domrec:<normalized domain>.
type DomainRecord struct {
	Raw            string   `json:"raw,omitempty"`
	ID             string   `json:"id,omitempty"`
	Domain         string   `json:"domain,omitempty"`
	Punycode       string   `json:"punycode,omitempty"`
	Name           string   `json:"name,omitempty"`
	Extension      string   `json:"extension,omitempty"`
	WhoisServer    string   `json:"whois_server,omitempty"`
	CreatedDate    string   `json:"created_date,omitempty"`
	UpdatedDate    string   `json:"updated_date,omitempty"`
	ExpirationDate string   `json:"expiration_date,omitempty"`
	Status         []string `json:"status,omitempty"`
	DNSSEC         *bool    `json:"dnssec,omitempty"`
}

// AssetType implements Asset.
func (DomainRecord) AssetType() string { return TypeDomainRecord }

// Key implements Asset.
func (a DomainRecord) Key() (string, error) {
	n, err := a.normalized()
	if err != nil {
		return "", err
	}
	return "domrec:" + n.Domain, nil
}

// Normalize implements Asset: domain is normalized as a hostname.
func (a DomainRecord) Normalize() (Asset, error) { return a.normalized() }

func (a DomainRecord) normalized() (DomainRecord, error) {
	d, err := netnorm.Hostname(a.Domain)
	if err != nil {
		return DomainRecord{}, assetErr(TypeDomainRecord, "domain", err)
	}
	a.Domain = d
	return a, nil
}

// MarshalJSON encodes the asset with its type discriminator.
func (a DomainRecord) MarshalJSON() ([]byte, error) {
	type plain DomainRecord
	return marshalTagged(TypeDomainRecord, plain(a))
}

// IPNetRecord is RDAP/WHOIS allocation data for an address range.
// Key: ipnet:<handle> when a handle is present (uppercased: registry
// handles are case-insensitive), otherwise ipnet:<normalized cidr>. The OAM
// field "type" is ipnet_type here because "type" is the asset
// discriminator.
type IPNetRecord struct {
	Raw          string   `json:"raw,omitempty"`
	CIDR         string   `json:"cidr,omitempty"`
	Handle       string   `json:"handle,omitempty"`
	StartAddress string   `json:"start_address,omitempty"`
	EndAddress   string   `json:"end_address,omitempty"`
	NetType      string   `json:"ipnet_type,omitempty"`
	Name         string   `json:"name,omitempty"`
	Method       string   `json:"method,omitempty"`
	Country      string   `json:"country,omitempty"`
	ParentHandle string   `json:"parent_handle,omitempty"`
	WhoisServer  string   `json:"whois_server,omitempty"`
	CreatedDate  string   `json:"created_date,omitempty"`
	UpdatedDate  string   `json:"updated_date,omitempty"`
	Status       []string `json:"status,omitempty"`
}

// AssetType implements Asset.
func (IPNetRecord) AssetType() string { return TypeIPNetRecord }

// Key implements Asset.
func (a IPNetRecord) Key() (string, error) {
	n, err := a.normalized()
	if err != nil {
		return "", err
	}
	if n.Handle != "" {
		return "ipnet:" + n.Handle, nil
	}
	return "ipnet:" + n.CIDR, nil
}

// Normalize implements Asset.
func (a IPNetRecord) Normalize() (Asset, error) { return a.normalized() }

func (a IPNetRecord) normalized() (IPNetRecord, error) {
	a.Handle = strings.ToUpper(strings.TrimSpace(a.Handle))
	if a.CIDR != "" {
		p, err := netnorm.CIDR(a.CIDR)
		if err != nil {
			return IPNetRecord{}, assetErr(TypeIPNetRecord, "cidr", err)
		}
		a.CIDR = p.String()
	}
	if a.Handle == "" && a.CIDR == "" {
		return IPNetRecord{}, assetErr(TypeIPNetRecord, "handle or cidr", nil)
	}
	return a, nil
}

// MarshalJSON encodes the asset with its type discriminator.
func (a IPNetRecord) MarshalJSON() ([]byte, error) {
	type plain IPNetRecord
	return marshalTagged(TypeIPNetRecord, plain(a))
}

// AutnumRecord is RDAP/WHOIS data for an AS number. Key: autnum:<number>.
type AutnumRecord struct {
	Raw         string   `json:"raw,omitempty"`
	Number      int64    `json:"number,omitempty"`
	Handle      string   `json:"handle,omitempty"`
	Name        string   `json:"name,omitempty"`
	WhoisServer string   `json:"whois_server,omitempty"`
	CreatedDate string   `json:"created_date,omitempty"`
	UpdatedDate string   `json:"updated_date,omitempty"`
	Status      []string `json:"status,omitempty"`
}

// AssetType implements Asset.
func (AutnumRecord) AssetType() string { return TypeAutnumRecord }

// Key implements Asset.
func (a AutnumRecord) Key() (string, error) {
	if err := checkASN(TypeAutnumRecord, a.Number); err != nil {
		return "", err
	}
	return "autnum:" + strconv.FormatInt(a.Number, 10), nil
}

// Normalize implements Asset.
func (a AutnumRecord) Normalize() (Asset, error) {
	if err := checkASN(TypeAutnumRecord, a.Number); err != nil {
		return nil, err
	}
	return a, nil
}

// MarshalJSON encodes the asset with its type discriminator.
func (a AutnumRecord) MarshalJSON() ([]byte, error) {
	type plain AutnumRecord
	return marshalTagged(TypeAutnumRecord, plain(a))
}
