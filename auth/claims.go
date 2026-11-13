package auth

import "time"

// Claims are the access and internal token claims of spec 13.2. Times are
// NumericDate seconds.
type Claims struct {
	Issuer         string `json:"iss"`
	Audience       string `json:"aud"`
	Subject        string `json:"sub"`           // usr_... in access tokens; the actor ("user:<uuid>", "service:<name>", ...) in internal tokens
	TenantID       string `json:"tid"`           // ten_...
	Role           Role   `json:"role"`          // membership role in TenantID
	SessionID      string `json:"sid,omitempty"` // ses_..., the refresh family
	SessionVersion int64  `json:"sv,omitempty"`  // user.session_version at issue
	ID             string `json:"jti"`
	IssuedAt       int64  `json:"iat"`
	ExpiresAt      int64  `json:"exp"`
	NotBefore      int64  `json:"nbf,omitempty"`
	// Groups are the tenant role groups of the actor (internal tokens
	// only): free-form approval roles such as "finance_ap" (spec 9.7) that
	// the gateway looked up for the caller. At most MaxGroups names, each
	// a ValidGroupName; the verifier rejects anything else (G-LIM-05).
	Groups []string `json:"groups,omitempty"`
}

// MaxGroups bounds the groups claim, so a token stays small (it travels
// in gRPC metadata on every call) and a tenant cannot make a member's
// tokens unmintable by accident: identity refuses the 33rd membership.
const MaxGroups = 32

// ValidGroupName reports whether name may be a role group: lower case
// [a-z][a-z0-9_.-]{0,63}, and not one of the membership roles, since a
// group "admin" would let a viewer match approvals addressed to admins
// (ApprovalRoleMatches compares groups and roles in one namespace).
func ValidGroupName(name string) bool {
	if len(name) < 1 || len(name) > 64 || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-') {
			return false
		}
	}
	_, err := ParseRole(name)
	return err != nil
}

// validGroups checks a groups claim.
func validGroups(gs []string) bool {
	if len(gs) > MaxGroups {
		return false
	}
	for _, g := range gs {
		if !ValidGroupName(g) {
			return false
		}
	}
	return true
}

// Expiry returns exp as a time.
func (c Claims) Expiry() time.Time { return time.Unix(c.ExpiresAt, 0).UTC() }

// Issued returns iat as a time.
func (c Claims) Issued() time.Time { return time.Unix(c.IssuedAt, 0).UTC() }
