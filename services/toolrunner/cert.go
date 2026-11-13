package toolrunner

import (
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/scope"
)

// timeText is the portable time form of tool output (RFC 3339 UTC with
// milliseconds, V2_DESIGN §1.6).
func timeText(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// keyUsageNames are the X.509 key usage bits in bit order (RFC 5280
// §4.2.1.3), with their RFC names in snake case.
var keyUsageNames = []struct {
	bit  x509.KeyUsage
	name string
}{
	{x509.KeyUsageDigitalSignature, "digital_signature"}, {x509.KeyUsageContentCommitment, "content_commitment"},
	{x509.KeyUsageKeyEncipherment, "key_encipherment"}, {x509.KeyUsageDataEncipherment, "data_encipherment"},
	{x509.KeyUsageKeyAgreement, "key_agreement"}, {x509.KeyUsageCertSign, "key_cert_sign"},
	{x509.KeyUsageCRLSign, "crl_sign"}, {x509.KeyUsageEncipherOnly, "encipher_only"}, {x509.KeyUsageDecipherOnly, "decipher_only"},
}

// signatureNames maps signature algorithms to their RFC names (RFC 4055,
// RFC 5758, RFC 8410), not Go's enum text.
var signatureNames = map[x509.SignatureAlgorithm]string{
	x509.SHA1WithRSA: "sha1WithRSAEncryption", x509.SHA256WithRSA: "sha256WithRSAEncryption",
	x509.SHA384WithRSA: "sha384WithRSAEncryption", x509.SHA512WithRSA: "sha512WithRSAEncryption",
	x509.SHA256WithRSAPSS: "rsassa-pss-sha256", x509.SHA384WithRSAPSS: "rsassa-pss-sha384", x509.SHA512WithRSAPSS: "rsassa-pss-sha512",
	x509.ECDSAWithSHA1: "ecdsa-with-SHA1", x509.ECDSAWithSHA256: "ecdsa-with-SHA256", x509.ECDSAWithSHA384: "ecdsa-with-SHA384",
	x509.ECDSAWithSHA512: "ecdsa-with-SHA512", x509.PureEd25519: "ed25519",
}

var publicKeyNames = map[x509.PublicKeyAlgorithm]string{x509.RSA: "rsa", x509.ECDSA: "ecdsa", x509.Ed25519: "ed25519", x509.DSA: "dsa"}

// certAsset maps a parsed certificate to the oam TLSCertificate (key: the
// SHA-256 of the DER).
func certAsset(c *x509.Certificate) oam.TLSCertificate {
	a := oam.TLSCertificate{
		Version:               strconv.Itoa(c.Version),
		SerialNumber:          serialText(c),
		SubjectCommonName:     c.Subject.CommonName,
		IssuerCommonName:      c.Issuer.CommonName,
		NotBefore:             timeText(c.NotBefore),
		NotAfter:              timeText(c.NotAfter),
		SignatureAlgorithm:    signatureNames[c.SignatureAlgorithm],
		PublicKeyAlgorithm:    publicKeyNames[c.PublicKeyAlgorithm],
		CRLDistributionPoints: slices.Clone(c.CRLDistributionPoints),
		FingerprintSHA256:     oam.TLSFingerprint(c.Raw),
	}
	if a.SignatureAlgorithm == "" {
		a.SignatureAlgorithm = "unknown"
	}
	if a.PublicKeyAlgorithm == "" {
		a.PublicKeyAlgorithm = "unknown"
	}
	for _, k := range keyUsageNames {
		if c.KeyUsage&k.bit != 0 {
			a.KeyUsage = append(a.KeyUsage, k.name)
		}
	}
	if c.BasicConstraintsValid {
		ca := c.IsCA
		a.IsCA = &ca
	}
	if len(c.SubjectKeyId) > 0 {
		a.SubjectKeyID = hex.EncodeToString(c.SubjectKeyId)
	}
	if len(c.AuthorityKeyId) > 0 {
		a.AuthorityKeyID = hex.EncodeToString(c.AuthorityKeyId)
	}
	return a
}

// serialText is the serial as lowercase hex of its big-endian octets (as
// crt.sh shows it); negative serials (invalid, but seen) keep a sign.
func serialText(c *x509.Certificate) string {
	if c.SerialNumber == nil {
		return ""
	}
	h := hex.EncodeToString(c.SerialNumber.Bytes())
	if h == "" {
		h = "00"
	}
	if c.SerialNumber.Sign() < 0 {
		return "-" + h
	}
	return h
}

// certRecord is the prelude TlsCertificate record (tool output).
func certRecord(a oam.TLSCertificate) map[string]any {
	return map[string]any{"version": a.Version, "serial_number": a.SerialNumber, "subject_common_name": a.SubjectCommonName,
		"issuer_common_name": a.IssuerCommonName, "not_before": a.NotBefore, "not_after": a.NotAfter, "is_ca": a.IsCA != nil && *a.IsCA}
}

// certName normalizes a certificate name for the graph: a leading "*."
// wildcard is replaced by its base name (the zone the wildcard covers);
// anything that is not a valid DNS name (IP SANs, e-mail addresses, other
// wildcards) is dropped.
func certName(s string) (string, bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimPrefix(s, "*.")
	if s == "" || strings.ContainsAny(s, "*@ /") {
		return "", false
	}
	t, err := scope.Normalize(s, scope.TargetHost)
	if err != nil || !t.IsName() {
		return "", false
	}
	return t.Name(), true
}

// certNames returns the normalized common name (or "") and the sorted,
// deduplicated DNS SANs of c.
func certNames(c *x509.Certificate) (string, []string) {
	cn, _ := certName(c.Subject.CommonName)
	var sans []string
	for _, n := range c.DNSNames {
		if v, ok := certName(n); ok {
			sans = append(sans, v)
		}
	}
	slices.Sort(sans)
	return cn, slices.Compact(sans)
}

// certObservation adds a certificate and its name edges to obs: the
// TLSCertificate under ref, an FQDN per name, common_name and san_dns_name
// edges.
func certObservation(obs *oam.Observation, ref string, c *x509.Certificate) oam.TLSCertificate {
	a := certAsset(c)
	obs.Entities = append(obs.Entities, oam.ObservedEntity{Ref: ref, Asset: a})
	cn, sans := certNames(c)
	refs := map[string]string{}
	name := func(n string) string {
		if r, ok := refs[n]; ok {
			return r
		}
		r := ref + ".n" + strconv.Itoa(len(refs))
		refs[n] = r
		obs.Entities = append(obs.Entities, oam.ObservedEntity{Ref: r, Asset: oam.FQDN{Name: n}})
		return r
	}
	if cn != "" {
		obs.Edges = append(obs.Edges, oam.ObservedEdge{From: ref, To: name(cn), Relation: oam.SimpleRelation{Label: oam.LabelCommonName}})
	}
	for _, s := range sans {
		obs.Edges = append(obs.Edges, oam.ObservedEdge{From: ref, To: name(s), Relation: oam.SimpleRelation{Label: oam.LabelSANDNSName}})
	}
	return a
}

// parseCertificate accepts PEM (the first CERTIFICATE block) or DER.
func parseCertificate(b []byte) (*x509.Certificate, error) {
	if blk, _ := pem.Decode(b); blk != nil {
		if blk.Type != "CERTIFICATE" {
			return nil, errors.New("not a certificate")
		}
		b = blk.Bytes
	}
	return x509.ParseCertificate(b)
}
