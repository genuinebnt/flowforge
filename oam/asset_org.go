package oam

import "strings"

// Organization is a company or other body. Key: org:<unique_id>.
type Organization struct {
	UniqueID       string   `json:"unique_id,omitempty"`
	Name           string   `json:"name,omitempty"`
	LegalName      string   `json:"legal_name,omitempty"`
	FoundingDate   string   `json:"founding_date,omitempty"`
	Jurisdiction   string   `json:"jurisdiction,omitempty"`
	RegistrationID string   `json:"registration_id,omitempty"`
	Industry       string   `json:"industry,omitempty"`
	TargetMarkets  []string `json:"target_markets,omitempty"`
	Active         *bool    `json:"active,omitempty"`
	NonProfit      *bool    `json:"non_profit,omitempty"`
	Headcount      int      `json:"headcount,omitempty"`
}

// AssetType implements Asset.
func (Organization) AssetType() string { return TypeOrganization }

// Key implements Asset.
func (a Organization) Key() (string, error) {
	n, err := a.normalized()
	if err != nil {
		return "", err
	}
	return "org:" + n.UniqueID, nil
}

// Normalize implements Asset: unique_id is trimmed.
func (a Organization) Normalize() (Asset, error) { return a.normalized() }

func (a Organization) normalized() (Organization, error) {
	a.UniqueID = strings.TrimSpace(a.UniqueID)
	if a.UniqueID == "" {
		return Organization{}, assetErr(TypeOrganization, "unique_id", nil)
	}
	return a, nil
}

// MarshalJSON encodes the asset with its type discriminator.
func (a Organization) MarshalJSON() ([]byte, error) {
	type plain Organization
	return marshalTagged(TypeOrganization, plain(a))
}
