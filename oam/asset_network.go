package oam

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/genuinebnt/flowforge/core/netnorm"
)

// IP version names used by IPAddress and Netblock.
const (
	IPv4 = "IPv4"
	IPv6 = "IPv6"
)

// maxASN is the largest 32-bit autonomous system number.
const maxASN = 1<<32 - 1

// FQDN is a fully qualified domain name. Key: fqdn:<normalized name>.
type FQDN struct {
	Name string `json:"name,omitempty"`
}

// AssetType implements Asset.
func (FQDN) AssetType() string { return TypeFQDN }

// Key implements Asset.
func (a FQDN) Key() (string, error) {
	n, err := a.normalized()
	if err != nil {
		return "", err
	}
	return "fqdn:" + n.Name, nil
}

// Normalize implements Asset: the name is normalized per spec 4.2.
func (a FQDN) Normalize() (Asset, error) { return a.normalized() }

func (a FQDN) normalized() (FQDN, error) {
	n, err := netnorm.Hostname(a.Name)
	if err != nil {
		return FQDN{}, assetErr(TypeFQDN, "name", err)
	}
	a.Name = n
	return a, nil
}

// MarshalJSON encodes the asset with its type discriminator.
func (a FQDN) MarshalJSON() ([]byte, error) {
	type plain FQDN
	return marshalTagged(TypeFQDN, plain(a))
}

// IPAddress is a single address. OAM's Go struct calls the version field
// Type; spec 5.4 renames it ip_version. Key: ip:<canonical text>.
type IPAddress struct {
	Address   string `json:"address,omitempty"`
	IPVersion string `json:"ip_version,omitempty"` // IPv4 or IPv6
}

// AssetType implements Asset.
func (IPAddress) AssetType() string { return TypeIPAddress }

// Key implements Asset.
func (a IPAddress) Key() (string, error) {
	n, err := a.normalized()
	if err != nil {
		return "", err
	}
	return "ip:" + n.Address, nil
}

// Normalize implements Asset: the address is made canonical (IPv4-mapped
// IPv6 becomes IPv4) and ip_version is filled in from it. A stated
// ip_version that contradicts the address is an error.
func (a IPAddress) Normalize() (Asset, error) { return a.normalized() }

func (a IPAddress) normalized() (IPAddress, error) {
	ip, err := netnorm.IP(a.Address)
	if err != nil {
		return IPAddress{}, assetErr(TypeIPAddress, "address", err)
	}
	v := IPv6
	if ip.Is4() {
		v = IPv4
	}
	if err := checkIPVersion(TypeIPAddress, a.IPVersion, v); err != nil {
		return IPAddress{}, err
	}
	a.Address, a.IPVersion = ip.String(), v
	return a, nil
}

// MarshalJSON encodes the asset with its type discriminator.
func (a IPAddress) MarshalJSON() ([]byte, error) {
	type plain IPAddress
	return marshalTagged(TypeIPAddress, plain(a))
}

// Netblock is a CIDR prefix. Key: net:<cidr with host bits zeroed>.
type Netblock struct {
	CIDR      string `json:"cidr,omitempty"`
	IPVersion string `json:"ip_version,omitempty"`
}

// AssetType implements Asset.
func (Netblock) AssetType() string { return TypeNetblock }

// Key implements Asset.
func (a Netblock) Key() (string, error) {
	n, err := a.normalized()
	if err != nil {
		return "", err
	}
	return "net:" + n.CIDR, nil
}

// Normalize implements Asset: host bits are zeroed and ip_version filled.
func (a Netblock) Normalize() (Asset, error) { return a.normalized() }

func (a Netblock) normalized() (Netblock, error) {
	p, err := netnorm.CIDR(a.CIDR)
	if err != nil {
		return Netblock{}, assetErr(TypeNetblock, "cidr", err)
	}
	v := IPv6
	if p.Addr().Is4() {
		v = IPv4
	}
	if err := checkIPVersion(TypeNetblock, a.IPVersion, v); err != nil {
		return Netblock{}, err
	}
	a.CIDR, a.IPVersion = p.String(), v
	return a, nil
}

// MarshalJSON encodes the asset with its type discriminator.
func (a Netblock) MarshalJSON() ([]byte, error) {
	type plain Netblock
	return marshalTagged(TypeNetblock, plain(a))
}

// AutonomousSystem is an AS number. Key: asn:<number>.
type AutonomousSystem struct {
	Number int64 `json:"number,omitempty"`
}

// AssetType implements Asset.
func (AutonomousSystem) AssetType() string { return TypeAutonomousSystem }

// Key implements Asset. AS 0 is rejected: it is reserved (RFC 7607) and
// indistinguishable from a missing number.
func (a AutonomousSystem) Key() (string, error) {
	if err := checkASN(TypeAutonomousSystem, a.Number); err != nil {
		return "", err
	}
	return "asn:" + strconv.FormatInt(a.Number, 10), nil
}

// Normalize implements Asset.
func (a AutonomousSystem) Normalize() (Asset, error) {
	if err := checkASN(TypeAutonomousSystem, a.Number); err != nil {
		return nil, err
	}
	return a, nil
}

// MarshalJSON encodes the asset with its type discriminator.
func (a AutonomousSystem) MarshalJSON() ([]byte, error) {
	type plain AutonomousSystem
	return marshalTagged(TypeAutonomousSystem, plain(a))
}

func checkASN(typ string, n int64) error {
	if n < 1 || n > maxASN {
		return assetErr(typ, "number", fmt.Errorf("%d outside 1..%d", n, int64(maxASN)))
	}
	return nil
}

func checkIPVersion(typ, stated, actual string) error {
	if stated == "" {
		return nil
	}
	switch strings.ToLower(stated) {
	case "ipv4":
		stated = IPv4
	case "ipv6":
		stated = IPv6
	default:
		return assetErr(typ, "ip_version", fmt.Errorf("%q is neither IPv4 nor IPv6", stated))
	}
	if stated != actual {
		return assetErr(typ, "ip_version", fmt.Errorf("%s contradicts a %s address", stated, actual))
	}
	return nil
}
