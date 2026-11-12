package auth

import "time"

// Claims are the access and internal token claims of spec 13.2. Times are
// NumericDate seconds.
type Claims struct {
	Issuer         string `json:"iss"`
	Audience       string `json:"aud"`
	Subject        string `json:"sub"`           // usr_..., or the actor of an internal token
	TenantID       string `json:"tid"`           // ten_...
	Role           Role   `json:"role"`          // membership role in TenantID
	SessionID      string `json:"sid,omitempty"` // ses_..., the refresh family
	SessionVersion int64  `json:"sv,omitempty"`  // user.session_version at issue
	ID             string `json:"jti"`
	IssuedAt       int64  `json:"iat"`
	ExpiresAt      int64  `json:"exp"`
	NotBefore      int64  `json:"nbf,omitempty"`
}

// Expiry returns exp as a time.
func (c Claims) Expiry() time.Time { return time.Unix(c.ExpiresAt, 0).UTC() }

// Issued returns iat as a time.
func (c Claims) Issued() time.Time { return time.Unix(c.IssuedAt, 0).UTC() }
