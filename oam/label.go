package oam

import (
	"slices"
	"strings"
)

// Relation labels with endpoint rules in spec 5.5 (source "docs").
const (
	LabelDNSRecord          = "dns_record"
	LabelPTRRecord          = "ptr_record"
	LabelContains           = "contains"
	LabelAnnounces          = "announces"
	LabelRegistration       = "registration"
	LabelPort               = "port"
	LabelCertificate        = "certificate"
	LabelProvider           = "provider"
	LabelProductUsed        = "product_used"
	LabelTermsOfService     = "terms_of_service"
	LabelDomain             = "domain"
	LabelIPAddress          = "ip_address"
	LabelFile               = "file"
	LabelSubsidiary         = "subsidiary"
	LabelOrgUnit            = "org_unit"
	LabelMember             = "member"
	LabelLegalAddress       = "legal_address"
	LabelHQAddress          = "hq_address"
	LabelLocation           = "location"
	LabelWebsite            = "website"
	LabelSocialMediaProfile = "social_media_profile"
	LabelID                 = "id"
	LabelAccount            = "account"
	LabelFundingSource      = "funding_source"
	LabelOwns               = "owns"
	// LabelCommonName and LabelSANDNSName are tagged "verify" in spec 5.5,
	// so their endpoints are not enforced.
	LabelCommonName = "common_name"
	LabelSANDNSName = "san_dns_name"
)

// Relation labels added by Flowforge v2 for content capture (V2 design
// §3.2). All are SimpleRelation; redirects_to may carry the HTTP status.
const (
	LabelLoadsScript     = "loads_script"
	LabelLoadsStyle      = "loads_style"
	LabelEmbeds          = "embeds"
	LabelLinksTo         = "links_to"
	LabelRedirectsTo     = "redirects_to"
	LabelFormAction      = "form_action"
	LabelCalls           = "calls"
	LabelSourceMap       = "source_map"
	LabelPreviousVersion = "previous_version"
	LabelIncludesLibrary = "includes_library"
	LabelDocuments       = "documents"
)

// EndpointRule lists the asset types a label may join. An empty To means
// any type.
type EndpointRule struct {
	From []string
	To   []string
}

// LabelRule returns the endpoint rule of a docs-tagged label. Labels
// without a rule (unknown labels, and labels tagged "verify") are accepted
// between any asset types.
func LabelRule(label string) (EndpointRule, bool) {
	r := func(from, to []string) (EndpointRule, bool) { return EndpointRule{From: from, To: to}, true }
	one := func(s ...string) []string { return s }
	switch strings.ToLower(label) {
	case LabelDNSRecord:
		return r(one(TypeFQDN), one(TypeIPAddress, TypeFQDN))
	case LabelPTRRecord:
		return r(one(TypeIPAddress), one(TypeFQDN))
	case LabelContains:
		return r(one(TypeNetblock), one(TypeIPAddress))
	case LabelAnnounces:
		return r(one(TypeAutonomousSystem), one(TypeNetblock))
	case LabelRegistration:
		return r(one(TypeNetblock), one(TypeIPNetRecord))
	case LabelPort:
		return r(one(TypeIPAddress, TypeFQDN, TypeURL), one(TypeService))
	case LabelCertificate:
		return r(one(TypeService), one(TypeTLSCertificate))
	case LabelProvider:
		return r(one(TypeService), one(TypeOrganization))
	case LabelProductUsed:
		return r(one(TypeService), one(TypeProduct, TypeProductRelease))
	case LabelTermsOfService:
		return r(one(TypeService), one(TypeFile, TypeURL))
	case LabelDomain:
		return r(one(TypeURL), one(TypeFQDN))
	case LabelIPAddress:
		return r(one(TypeURL), one(TypeIPAddress))
	case LabelFile:
		return r(one(TypeURL), one(TypeFile))
	case LabelSubsidiary, LabelOrgUnit:
		return r(one(TypeOrganization), one(TypeOrganization))
	case LabelMember:
		return r(one(TypeOrganization), one(TypePerson))
	case LabelLegalAddress, LabelHQAddress, LabelLocation:
		return r(one(TypeOrganization), one(TypeLocation))
	case LabelWebsite, LabelSocialMediaProfile:
		return r(one(TypeOrganization), one(TypeURL))
	case LabelID:
		return r(one(TypeOrganization), one(TypeIdentifier))
	case LabelAccount:
		return r(one(TypeOrganization), one(TypeAccount))
	case LabelFundingSource:
		return r(one(TypeOrganization), one(TypeOrganization, TypePerson))
	case LabelOwns:
		return r(one(TypeOrganization), nil)
	case LabelLoadsScript:
		return r(one(TypeURL), one(TypeFile))
	case LabelLoadsStyle:
		return r(one(TypeURL, TypeFile), one(TypeFile))
	case LabelEmbeds, LabelLinksTo, LabelRedirectsTo:
		return r(one(TypeURL), one(TypeURL))
	case LabelFormAction:
		return r(one(TypeURL), one(TypeEndpoint))
	case LabelCalls:
		return r(one(TypeFile, TypeURL), one(TypeEndpoint))
	case LabelSourceMap, LabelPreviousVersion:
		return r(one(TypeFile), one(TypeFile))
	case LabelIncludesLibrary:
		return r(one(TypeFile), one(TypeProductRelease))
	case LabelDocuments:
		return r(one(TypeURL), one(TypeEndpoint))
	}
	return EndpointRule{}, false
}

// ValidateEdge checks the label of rel against the asset types of its
// endpoints (spec 5.5). Only endpoint types of docs-tagged labels are
// validated; unknown labels on a known relation type are accepted. A
// violation is returned as an *EdgeError (errors.Is ErrLabelEndpoints).
func ValidateEdge(rel Relation, fromType, toType string) error {
	if rel == nil {
		return ErrInvalidRelation
	}
	label := strings.ToLower(rel.RelationLabel())
	rule, ok := LabelRule(label)
	if !ok {
		return nil
	}
	if slices.Contains(rule.From, fromType) && (len(rule.To) == 0 || slices.Contains(rule.To, toType)) {
		return nil
	}
	return &EdgeError{Label: label, FromType: fromType, ToType: toType, AllowedFrom: rule.From, AllowedTo: rule.To}
}
