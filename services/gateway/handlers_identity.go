package gateway

import (
	"crypto/rand"
	"net/http"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/services/gateway/identity"
)

// prefixInvite is the public-id prefix of invites (not in core's list).
const prefixInvite core.Prefix = "inv"

func (c *call) client() identity.ClientInfo {
	return identity.ClientInfo{IP: c.ip, UserAgent: truncate(c.r.UserAgent(), 256)}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (c *call) actor() identity.Actor { return c.p.identityActor(c.ip, truncate(c.r.UserAgent(), 256)) }

func (s *Server) signup(c *call) error {
	var in struct {
		Email      string `json:"email"`
		Password   string `json:"password"`
		TenantName string `json:"tenant_name"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	res, err := s.cfg.Identity.Signup(c.ctx, identity.SignupInput{Email: in.Email, Password: in.Password, TenantName: in.TenantName}, c.client())
	if err != nil {
		return err
	}
	return c.json(http.StatusAccepted, map[string]any{
		"user_id": core.PublicID(core.PrefixUser, res.UserID), "tenant_id": core.PublicID(core.PrefixTenant, res.TenantID),
		"status": "pending", "message": "Check your email to verify the address.",
	})
}

func (s *Server) verifyEmail(c *call) error {
	var in struct {
		Token string `json:"token"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	u, err := s.cfg.Identity.VerifyEmail(c.ctx, in.Token, c.client())
	if err != nil {
		return err
	}
	return c.ok(map[string]any{"user_id": core.PublicID(core.PrefixUser, u.ID), "status": string(u.Status), "email_verified_at": timeOrNull(u.EmailVerifiedAt)})
}

func (s *Server) resendVerification(c *call) error {
	var in struct {
		Email string `json:"email"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	if err := s.cfg.Identity.ResendVerification(c.ctx, in.Email, c.client()); err != nil {
		return err
	}
	return c.json(http.StatusAccepted, map[string]any{"message": "If the address is pending verification, a new email is on its way."})
}

// sessionJSON is the body of login and refresh responses (spec 12.4).
func sessionJSON(sess identity.Session, withRefresh bool) map[string]any {
	m := map[string]any{
		"access_token": sess.AccessToken, "token_type": "Bearer", "expires_in": int(sess.ExpiresIn / time.Second),
		"user":   map[string]any{"id": core.PublicID(core.PrefixUser, sess.User.ID), "email": sess.User.Email},
		"tenant": map[string]any{"id": core.PublicID(core.PrefixTenant, sess.Tenant.ID), "name": sess.Tenant.Name, "role": string(sess.Role)},
	}
	if withRefresh {
		m["refresh_token"] = sess.RefreshToken
		m["refresh_expires_at"] = core.FormatTime(sess.RefreshExpires)
	}
	return m
}

func (s *Server) writeSession(c *call, sess identity.Session, cookies bool) error {
	if cookies {
		csrf, err := auth.NewCSRFToken(rand.Reader)
		if err != nil {
			return err
		}
		s.setSessionCookies(c.w, sess, csrf)
	}
	return c.ok(sessionJSON(sess, !cookies))
}

func (s *Server) login(c *call) error {
	var in struct {
		Email      string `json:"email"`
		Password   string `json:"password"`
		TenantID   string `json:"tenant_id"`
		UseCookies bool   `json:"use_cookies"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	var tid core.UUID
	if in.TenantID != "" {
		var err error
		if tid, err = core.ParsePublicID(core.PrefixTenant, in.TenantID); err != nil {
			return errValidation("tenant_id: must be a ten_ identifier")
		}
	}
	sess, err := s.cfg.Identity.Login(c.ctx, identity.LoginInput{Email: in.Email, Password: in.Password, TenantID: tid}, c.client())
	if err != nil {
		return err
	}
	return s.writeSession(c, sess, in.UseCookies)
}

// refresh rotates the refresh token from the body (API/CLI) or the ff_rt
// cookie (console; CSRF-checked, answered with cookies).
func (s *Server) refresh(c *call) error {
	var in struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := c.decodeOptional(&in); err != nil {
		return err
	}
	cookie := false
	if in.RefreshToken == "" {
		ck, err := c.r.Cookie(CookieRefresh)
		if err != nil || ck.Value == "" {
			return errUnauth(core.ReasonTokenMissing, "refresh token required (body refresh_token or ff_rt cookie)")
		}
		if err := checkCSRF(c.r); err != nil {
			return err
		}
		in.RefreshToken, cookie = ck.Value, true
	}
	sess, err := s.cfg.Identity.Refresh(c.ctx, in.RefreshToken)
	if err != nil {
		if cookie {
			s.clearSessionCookies(c.w)
		}
		return err
	}
	return s.writeSession(c, sess, cookie)
}

// logout revokes the current session family: from the access token (sid)
// and/or the refresh token (body or ff_rt cookie). Cookies are cleared.
func (s *Server) logout(c *call) error {
	var in struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := c.decodeOptional(&in); err != nil {
		return err
	}
	if in.RefreshToken == "" {
		if ck, err := c.r.Cookie(CookieRefresh); err == nil && ck.Value != "" {
			if c.p == nil || !c.p.ViaCookie {
				if err := checkCSRF(c.r); err != nil {
					return err
				}
			}
			in.RefreshToken = ck.Value
		}
	}
	var claims *auth.Claims
	if c.p != nil {
		claims = c.p.Claims
	}
	if claims == nil && in.RefreshToken == "" {
		if c.p != nil {
			return c.noContent() // API keys have no session
		}
		return errUnauth(core.ReasonTokenMissing, "authentication required")
	}
	if err := s.cfg.Identity.Logout(c.ctx, identity.LogoutInput{Claims: claims, RefreshToken: in.RefreshToken}, c.client()); err != nil {
		return err
	}
	s.clearSessionCookies(c.w)
	return c.noContent()
}

func (s *Server) forgotPassword(c *call) error {
	var in struct {
		Email string `json:"email"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	if err := s.cfg.Identity.ForgotPassword(c.ctx, in.Email, c.client()); err != nil {
		return err
	}
	return c.json(http.StatusAccepted, map[string]any{"message": "If the address belongs to an account, a reset link is on its way."})
}

func (s *Server) resetPassword(c *call) error {
	var in struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	if err := s.cfg.Identity.ResetPassword(c.ctx, in.Token, in.Password, c.client()); err != nil {
		return err
	}
	return c.ok(map[string]any{"message": "Password updated. All sessions were signed out."})
}

func (s *Server) acceptInvite(c *call) error {
	var in struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	acc, err := s.cfg.Identity.AcceptInvite(c.ctx, in.Token, in.Password, c.client())
	if err != nil {
		return err
	}
	return c.ok(map[string]any{"user_id": core.PublicID(core.PrefixUser, acc.UserID), "tenant_id": core.PublicID(core.PrefixTenant, acc.TenantID),
		"role": string(acc.Role), "created": acc.Created})
}

func (s *Server) me(c *call) error {
	p, err := s.cfg.Identity.Me(c.ctx, c.actor())
	if err != nil {
		return err
	}
	perms := []string{}
	for _, pm := range auth.PermissionsOf(p.Role) {
		perms = append(perms, string(pm))
	}
	out := map[string]any{
		"user": nil, "api_key": nil, "role": string(p.Role), "permissions": perms,
		"tenant": map[string]any{"id": core.PublicID(core.PrefixTenant, p.Tenant.ID), "name": p.Tenant.Name, "slug": p.Tenant.Slug, "plan": p.Tenant.Plan, "role": string(p.Role)},
	}
	if u := p.User; u != nil {
		out["user"] = map[string]any{"id": core.PublicID(core.PrefixUser, u.ID), "email": u.Email, "status": string(u.Status),
			"email_verified_at": timeOrNull(u.EmailVerifiedAt), "created_at": core.FormatTime(u.CreatedAt)}
	}
	if k := p.APIKey; k != nil {
		out["api_key"] = map[string]any{"id": k.PublicID(), "name": k.Name}
	}
	return c.ok(out)
}

func memberJSON(m identity.Member) map[string]any {
	return map[string]any{"user_id": core.PublicID(core.PrefixUser, m.UserID), "email": m.Email, "role": string(m.Role),
		"status": string(m.Status), "created_at": core.FormatTime(m.CreatedAt)}
}

func (s *Server) listMembers(c *call) error {
	size, tok, err := c.page()
	if err != nil {
		return err
	}
	ms, next, err := s.cfg.Identity.ListMembers(c.ctx, c.p.TenantID, tok, int(size))
	if err != nil {
		return err
	}
	items := make([]map[string]any, 0, len(ms))
	for _, m := range ms {
		items = append(items, memberJSON(m))
	}
	return c.ok(listOf(items, next))
}

func (c *call) userParam() (core.UUID, error) {
	id, err := c.pathID("user_id", core.PrefixUser)
	if err != nil {
		return core.Nil, err
	}
	u, err := core.ParsePublicID(core.PrefixUser, id)
	if err != nil {
		return core.Nil, errNotFound()
	}
	return u, nil
}

func (s *Server) getMember(c *call) error {
	uid, err := c.userParam()
	if err != nil {
		return err
	}
	m, err := s.cfg.Identity.GetMember(c.ctx, c.p.TenantID, uid)
	if err != nil {
		return err
	}
	out := memberJSON(m)
	c.w.Header().Set("ETag", etag(out))
	return c.ok(out)
}

func (s *Server) patchMember(c *call) error {
	uid, err := c.userParam()
	if err != nil {
		return err
	}
	var in struct {
		Role string `json:"role"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	cur, err := s.cfg.Identity.GetMember(c.ctx, c.p.TenantID, uid)
	if err != nil {
		return err
	}
	if err := c.checkIfMatch(memberJSON(cur)); err != nil {
		return err
	}
	m, err := s.cfg.Identity.SetMemberRole(c.ctx, c.actor(), uid, auth.Role(in.Role))
	if err != nil {
		return err
	}
	out := memberJSON(m)
	c.w.Header().Set("ETag", etag(out))
	return c.ok(out)
}

func (s *Server) createInvite(c *call) error {
	var in struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	inv, err := s.cfg.Identity.CreateInvite(c.ctx, c.actor(), in.Email, auth.Role(in.Role))
	if err != nil {
		return err
	}
	return c.json(http.StatusCreated, map[string]any{"id": core.PublicID(prefixInvite, inv.ID), "email": inv.Email,
		"role": string(inv.Role), "expires_at": core.FormatTime(inv.ExpiresAt), "created_at": core.FormatTime(inv.CreatedAt)})
}

func apiKeyJSON(k identity.APIKeyRecord) map[string]any {
	return map[string]any{"id": k.PublicID(), "name": k.Name, "role": string(k.Role), "prefix": k.Prefix,
		"created_by": core.PublicID(core.PrefixUser, k.CreatedBy), "created_at": core.FormatTime(k.CreatedAt),
		"last_used_at": timeOrNull(k.LastUsedAt), "expires_at": timeOrNull(k.ExpiresAt), "revoked_at": timeOrNull(k.RevokedAt)}
}

func (s *Server) listAPIKeys(c *call) error {
	size, tok, err := c.page()
	if err != nil {
		return err
	}
	ks, next, err := s.cfg.Identity.ListAPIKeys(c.ctx, c.p.TenantID, tok, int(size))
	if err != nil {
		return err
	}
	items := make([]map[string]any, 0, len(ks))
	for _, k := range ks {
		items = append(items, apiKeyJSON(k))
	}
	return c.ok(listOf(items, next))
}

func (s *Server) createAPIKey(c *call) error {
	var in struct {
		Name      string `json:"name"`
		Role      string `json:"role"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	var exp time.Time
	if in.ExpiresAt != "" {
		var err error
		if exp, err = core.ParseTime(in.ExpiresAt); err != nil {
			return errValidation("expires_at: must be an RFC 3339 timestamp")
		}
	}
	k, err := s.cfg.Identity.CreateAPIKey(c.ctx, c.actor(), in.Name, auth.Role(in.Role), exp)
	if err != nil {
		return err
	}
	out := apiKeyJSON(k.Record)
	out["key"] = k.Token
	out["note"] = "This value is shown once."
	return c.json(http.StatusCreated, out)
}

func (s *Server) deleteAPIKey(c *call) error {
	id, err := c.pathID("id", core.PrefixAPIKey)
	if err != nil {
		return err
	}
	u, err := core.ParsePublicID(core.PrefixAPIKey, id)
	if err != nil {
		return errNotFound()
	}
	if err := s.cfg.Identity.RevokeAPIKey(c.ctx, c.actor(), u); err != nil {
		return err
	}
	return c.noContent()
}

// ---- role groups (G-LIM-05) ---------------------------------------------

// listRoleGroups is GET /v1/tenant/role-groups: every group with its
// members ({"items": [{"name", "members": [usr_...]}]}, not paginated: a
// tenant has few groups and each member at most auth.MaxGroups).
func (s *Server) listRoleGroups(c *call) error {
	gs, err := s.cfg.Identity.ListRoleGroups(c.ctx, c.p.TenantID)
	if err != nil {
		return err
	}
	items := make([]map[string]any, 0, len(gs))
	for _, g := range gs {
		ms := make([]string, 0, len(g.Members))
		for _, u := range g.Members {
			ms = append(ms, core.PublicID(core.PrefixUser, u))
		}
		items = append(items, map[string]any{"name": g.Name, "members": ms})
	}
	return c.ok(listOf(items, ""))
}

// putRoleGroupMember is PUT /v1/tenant/role-groups/{name}/members/{user_id}
// (idempotent: 204 whether or not the user was already a member).
func (s *Server) putRoleGroupMember(c *call) error {
	uid, err := c.userParam()
	if err != nil {
		return err
	}
	if err := s.cfg.Identity.AddRoleGroupMember(c.ctx, c.actor(), c.r.PathValue("name"), uid); err != nil {
		return err
	}
	return c.noContent()
}

// deleteRoleGroupMember is DELETE /v1/tenant/role-groups/{name}/members/{user_id}.
func (s *Server) deleteRoleGroupMember(c *call) error {
	uid, err := c.userParam()
	if err != nil {
		return err
	}
	if err := s.cfg.Identity.RemoveRoleGroupMember(c.ctx, c.actor(), c.r.PathValue("name"), uid); err != nil {
		return err
	}
	return c.noContent()
}
