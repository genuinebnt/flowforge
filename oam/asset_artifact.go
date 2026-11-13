package oam

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/core/netnorm"
)

// URL is a web address. Key: url:<normalized url without fragment>.
// The url field must not carry userinfo (netnorm rejects it); credentials,
// if a tool reports them at all, go in username and password.
type URL struct {
	URL      string `json:"url,omitempty"`
	Scheme   string `json:"scheme,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	Path     string `json:"path,omitempty"`
	Options  string `json:"options,omitempty"`
	Fragment string `json:"fragment,omitempty"`
}

// AssetType implements Asset.
func (URL) AssetType() string { return TypeURL }

// Key implements Asset.
func (a URL) Key() (string, error) {
	n, err := a.normalized()
	if err != nil {
		return "", err
	}
	return "url:" + n.URL, nil
}

// Normalize implements Asset: the url field is replaced by its canonical
// form (spec 4.2), which drops the fragment; the fragment field is kept.
func (a URL) Normalize() (Asset, error) { return a.normalized() }

func (a URL) normalized() (URL, error) {
	u, err := netnorm.ParseURL(a.URL)
	if err != nil {
		return URL{}, assetErr(TypeURL, "url", err)
	}
	if a.Port < 0 || a.Port > 65535 {
		return URL{}, assetErr(TypeURL, "port", fmt.Errorf("%d out of range", a.Port))
	}
	a.URL = u.String()
	return a, nil
}

// MarshalJSON encodes the asset with its type discriminator.
func (a URL) MarshalJSON() ([]byte, error) {
	type plain URL
	return marshalTagged(TypeURL, plain(a))
}

// Service is a network service bound to a port. Key: svc:<unique_id>; see
// ServiceUniqueID for how adapters derive unique_id.
type Service struct {
	UniqueID     string              `json:"unique_id,omitempty"`
	ServiceType  string              `json:"service_type,omitempty"`
	Output       string              `json:"output,omitempty"`
	OutputLength int                 `json:"output_length,omitempty"`
	Attributes   map[string][]string `json:"attributes,omitempty"`
}

// ServiceUniqueID returns the spec 5.4 unique_id of a service:
// sha256(parent_key + "/" + port + "/" + protocol + "/" + service_type) in
// lowercase hex. Protocol and service type are lowercased first so that
// "TCP" and "tcp" agree.
func ServiceUniqueID(parentKey string, port int, protocol, serviceType string) string {
	s := parentKey + "/" + strconv.Itoa(port) + "/" + strings.ToLower(protocol) + "/" + strings.ToLower(serviceType)
	return core.SHA256Hex([]byte(s))
}

// AssetType implements Asset.
func (Service) AssetType() string { return TypeService }

// Key implements Asset.
func (a Service) Key() (string, error) {
	n, err := a.normalized()
	if err != nil {
		return "", err
	}
	return "svc:" + n.UniqueID, nil
}

// Normalize implements Asset: unique_id is trimmed, and lowercased when it
// is hexadecimal.
func (a Service) Normalize() (Asset, error) { return a.normalized() }

func (a Service) normalized() (Service, error) {
	id := strings.TrimSpace(a.UniqueID)
	if id == "" {
		return Service{}, assetErr(TypeService, "unique_id", nil)
	}
	if isHex(id) {
		id = strings.ToLower(id)
	}
	if a.OutputLength < 0 {
		return Service{}, assetErr(TypeService, "output_length", errors.New("negative"))
	}
	a.UniqueID = id
	return a, nil
}

// MarshalJSON encodes the asset with its type discriminator.
func (a Service) MarshalJSON() ([]byte, error) {
	type plain Service
	return marshalTagged(TypeService, plain(a))
}

// TLSCertificate is an X.509 certificate. Key: tls:<sha256 of DER>. The
// spec field list has no DER, so the hash travels in the Flowforge field
// fingerprint_sha256 (see OPEN_QUESTIONS.md); use TLSFingerprint to compute
// it. The other field names are OAM's (certificate.TLSCertificate,
// G-CORE-02), ext_key_usage included; key_usage and ext_key_usage hold
// OAM's usage names (KeyUsage*, ExtKeyUsage* constants).
type TLSCertificate struct {
	Version               string   `json:"version,omitempty"`
	SerialNumber          string   `json:"serial_number,omitempty"`
	SubjectCommonName     string   `json:"subject_common_name,omitempty"`
	IssuerCommonName      string   `json:"issuer_common_name,omitempty"`
	NotBefore             string   `json:"not_before,omitempty"`
	NotAfter              string   `json:"not_after,omitempty"`
	KeyUsage              []string `json:"key_usage,omitempty"`
	ExtKeyUsage           []string `json:"ext_key_usage,omitempty"`
	SignatureAlgorithm    string   `json:"signature_algorithm,omitempty"`
	PublicKeyAlgorithm    string   `json:"public_key_algorithm,omitempty"`
	IsCA                  *bool    `json:"is_ca,omitempty"`
	CRLDistributionPoints []string `json:"crl_distribution_points,omitempty"`
	SubjectKeyID          string   `json:"subject_key_id,omitempty"`
	AuthorityKeyID        string   `json:"authority_key_id,omitempty"`
	FingerprintSHA256     string   `json:"fingerprint_sha256,omitempty"` // ff
}

// Key usage names (RFC 5280 §4.2.1.3) as OAM spells them
// (open-asset-model certificate/tls_certificate.go), in bit order.
const (
	KeyUsageDigitalSignature  = "Digital Signature"
	KeyUsageContentCommitment = "Content Commitment"
	KeyUsageKeyEncipherment   = "Key Encipherment"
	KeyUsageDataEncipherment  = "Data Encipherment"
	KeyUsageKeyAgreement      = "Key Agreement"
	KeyUsageCertSign          = "Certificate Sign"
	KeyUsageCRLSign           = "CRL Sign"
	KeyUsageEncipherOnly      = "Encipher Only"
	KeyUsageDecipherOnly      = "Decipher Only"
)

// Extended key usage names (RFC 5280 §4.2.1.12) as OAM spells them.
const (
	ExtKeyUsageAny                            = "Any Usage"
	ExtKeyUsageServerAuth                     = "TLS Server Authentication"
	ExtKeyUsageClientAuth                     = "TLS Client Authentication"
	ExtKeyUsageCodeSigning                    = "Code Signing"
	ExtKeyUsageEmailProtection                = "E-mail Protection"
	ExtKeyUsageIPSECEndSystem                 = "IPSec End System"
	ExtKeyUsageIPSECTunnel                    = "IPSec Tunnel"
	ExtKeyUsageIPSECUser                      = "IPSec User"
	ExtKeyUsageTimeStamping                   = "Trusted Timestamping"
	ExtKeyUsageOCSPSigning                    = "OCSP Signing"
	ExtKeyUsageMicrosoftServerGatedCrypto     = "Microsoft Server Gated Crypto"
	ExtKeyUsageNetscapeServerGatedCrypto      = "Netscape Server Gated Crypto"
	ExtKeyUsageMicrosoftCommercialCodeSigning = "Microsoft Commercial Code Signing"
	ExtKeyUsageMicrosoftKernelCodeSigning     = "Microsoft Kernel Code Signing"
)

// TLSFingerprint returns the lowercase hex SHA-256 of a DER certificate.
func TLSFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// AssetType implements Asset.
func (TLSCertificate) AssetType() string { return TypeTLSCertificate }

// Key implements Asset.
func (a TLSCertificate) Key() (string, error) {
	n, err := a.normalized()
	if err != nil {
		return "", err
	}
	return "tls:" + n.FingerprintSHA256, nil
}

// Normalize implements Asset: the fingerprint loses colons and spaces and
// is lowercased; it must then be 64 hex digits.
func (a TLSCertificate) Normalize() (Asset, error) { return a.normalized() }

func (a TLSCertificate) normalized() (TLSCertificate, error) {
	fp := strings.ToLower(strings.NewReplacer(":", "", " ", "").Replace(a.FingerprintSHA256))
	if fp == "" {
		return TLSCertificate{}, assetErr(TypeTLSCertificate, "fingerprint_sha256", nil)
	}
	if len(fp) != 64 || !isHex(fp) {
		return TLSCertificate{}, assetErr(TypeTLSCertificate, "fingerprint_sha256", errors.New("not a SHA-256 hex digest"))
	}
	a.FingerprintSHA256 = fp
	return a, nil
}

// MarshalJSON encodes the asset with its type discriminator.
func (a TLSCertificate) MarshalJSON() ([]byte, error) {
	type plain TLSCertificate
	return marshalTagged(TypeTLSCertificate, plain(a))
}

// File is a document served at a URL. Key: file:<sha256 of url>, where an
// http(s) url is normalized first and any other url is only trimmed. The
// OAM field "type" is file_type here because "type" is the asset
// discriminator.
type File struct {
	URL      string `json:"url,omitempty"`
	Name     string `json:"name,omitempty"`
	FileType string `json:"file_type,omitempty"`
}

// AssetType implements Asset.
func (File) AssetType() string { return TypeFile }

// Key implements Asset.
func (a File) Key() (string, error) {
	n, err := a.normalized()
	if err != nil {
		return "", err
	}
	return "file:" + core.SHA256Hex([]byte(n.URL)), nil
}

// Normalize implements Asset.
func (a File) Normalize() (Asset, error) { return a.normalized() }

func (a File) normalized() (File, error) {
	u := strings.TrimSpace(a.URL)
	if u == "" {
		return File{}, assetErr(TypeFile, "url", nil)
	}
	if pu, err := netnorm.ParseURL(u); err == nil {
		u = pu.String()
	}
	a.URL = u
	return a, nil
}

// MarshalJSON encodes the asset with its type discriminator.
func (a File) MarshalJSON() ([]byte, error) {
	type plain File
	return marshalTagged(TypeFile, plain(a))
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
