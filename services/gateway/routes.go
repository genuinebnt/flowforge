package gateway

import (
	"net/http"

	"github.com/genuinebnt/flowforge/auth"
)

// routes is the endpoint table (spec 12.3 and the 22.9 additions). The
// permission column is the 13.6 matrix (conformance/auth/rbac/matrix.json);
// it is checked before any internal service is called.
func (s *Server) routes() []route {
	const (
		get, post, put, patch, del = http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete
		read                       = auth.PermRead
		wf                         = auth.PermWorkflowsWrite
		runs                       = auth.PermRunsSubmit
		decide                     = auth.PermApprovalsDecide
		alerts                     = auth.PermAlertsWrite
		programs                   = auth.PermProgramsAdmin
		members                    = auth.PermMembersAdmin
	)
	pub := func(m, p string, h func(*call) error) route {
		return route{method: m, pattern: "/v1" + p, public: true, class: ClassAuth, h: h}
	}
	r := func(m, p string, perm auth.Permission, h func(*call) error) route {
		class := ClassRead
		if m != get {
			class = ClassWrite
		}
		return route{method: m, pattern: "/v1" + p, perm: perm, class: class, idem: idemOptional, h: h}
	}
	sse := func(p string, h func(*call) error) route {
		return route{method: get, pattern: "/v1" + p, perm: read, class: ClassStream, stream: true, h: h}
	}
	probe := func(p string, h func(*call) error) []route {
		return []route{{method: get, pattern: p, public: true, h: h}, {method: get, pattern: "/v1" + p, public: true, h: h}}
	}

	rs := []route{
		// Identity (12.4, 13).
		pub(post, "/auth/signup", s.signup),
		pub(post, "/auth/verify-email", s.verifyEmail),
		pub(post, "/auth/verify-email/:resend", s.resendVerification),
		pub(post, "/auth/login", s.login),
		pub(post, "/auth/refresh", s.refresh),
		{method: post, pattern: "/v1/auth/logout", public: true, optionalAuth: true, class: ClassAuth, h: s.logout},
		pub(post, "/auth/password/forgot", s.forgotPassword),
		pub(post, "/auth/password/reset", s.resetPassword),
		pub(post, "/auth/invites/:accept", s.acceptInvite),
		r(get, "/me", "", s.me),
		r(get, "/tenant/members", members, s.listMembers),
		r(get, "/tenant/members/{user_id}", members, s.getMember),
		r(patch, "/tenant/members/{user_id}", members, s.patchMember),
		r(post, "/tenant/invites", members, s.createInvite),
		// Role groups for free-form approval roles (G-LIM-05; not in 13.6,
		// so members:admin like the other membership endpoints).
		r(get, "/tenant/role-groups", members, s.listRoleGroups),
		r(put, "/tenant/role-groups/{name}/members/{user_id}", members, s.putRoleGroupMember),
		r(del, "/tenant/role-groups/{name}/members/{user_id}", members, s.deleteRoleGroupMember),
		r(get, "/api-keys", members, s.listAPIKeys),
		r(post, "/api-keys", members, s.createAPIKey),
		r(del, "/api-keys/{id}", members, s.deleteAPIKey),

		// Programs and scope (12.5).
		r(get, "/programs", read, s.listPrograms),
		r(post, "/programs", programs, s.createProgram),
		r(get, "/programs/{id}", read, s.getProgram),
		r(patch, "/programs/{id}", programs, s.patchProgram),
		r(get, "/programs/{id}/scope", read, s.getScope),
		r(put, "/programs/{id}/scope", programs, s.putScope),
		r(get, "/programs/{id}/scope/versions", read, s.listScopeVersions),
		r(post, "/programs/{id}/scope/:check", wf, s.checkScope),
		r(post, "/programs/{id}/kill-switch", programs, s.killSwitch),

		// Workflows, tools and runs (12.6).
		r(post, "/workflows/:check", wf, s.checkWorkflow),
		r(post, "/workflows", wf, s.registerWorkflow),
		r(get, "/workflows", read, s.listWorkflows),
		r(get, "/workflows/{name}/versions/{v}", read, s.getWorkflowVersion),
		r(get, "/tools", read, s.listTools),
		{method: post, pattern: "/v1/runs", perm: runs, class: ClassWrite, idem: idemRequired, h: s.submitRun},
		r(get, "/runs", read, s.listRuns),
		r(get, "/runs/{id}", read, s.getRun),
		r(post, "/runs/{id}/:cancel", runs, s.cancelRun),
		r(get, "/runs/{id}/events", read, s.listRunEvents),
		r(post, "/schedules", runs, s.createSchedule),
		r(get, "/schedules", read, s.listSchedules),
		r(get, "/schedules/{id}", read, s.getSchedule),
		r(del, "/schedules/{id}", runs, s.deleteSchedule),
		sse("/runs/{id}/watch", s.watchRun),

		// Approvals (12.7).
		r(get, "/approvals", read, s.listApprovals),
		r(get, "/approvals/{id}", read, s.getApproval),
		r(post, "/approvals/{id}/:decide", decide, s.decideApproval),

		// Graph (12.7, 21.6, 22.9).
		r(get, "/graph/entities", read, s.listEntities),
		r(get, "/graph/entities/{id}", read, s.getEntity),
		r(get, "/graph/entities/{id}/neighbors", read, s.neighbors),
		r(get, "/graph/entities/{id}/similar", read, s.similarEntity),
		r(post, "/graph/similar", read, s.similarText),
		r(post, "/graph/analysis", runs, s.analysis),
		r(get, "/graph/snapshots", read, s.snapshots),
		r(get, "/graph/search", read, s.search),
		r(post, "/graph/paths", read, s.paths),
		r(get, "/graph/diff", read, s.diff),
		sse("/graph/changes/watch", s.watchChanges),
		r(get, "/programs/{pid}/entities/{id}/similar", read, s.similarEntity),
		r(post, "/programs/{pid}/similar", read, s.similarText),
		r(post, "/programs/{pid}/graph/analysis", runs, s.analysis),
		r(get, "/programs/{pid}/graph/snapshots", read, s.snapshots),
		r(get, "/programs/{pid}/similarity/index", programs, s.similarityIndex),

		// Content (V2 design §7, §11 items 13, 14, 18).
		r(get, "/snapshots", read, s.listSnapshots),
		r(get, "/snapshots/{id}", read, s.getSnapshot),
		r(get, "/snapshots/{a}/diff/{b}", read, s.diffSnapshots),
		r(post, "/snapshots/{id}/:pin", alerts, s.pinSnapshot),
		r(post, "/snapshots/{id}/:unpin", alerts, s.unpinSnapshot),
		r(get, "/changes/search", read, s.searchChanges),
		r(get, "/changes/first-appeared", read, s.firstAppeared),
		r(get, "/content/quota", read, s.contentQuota),

		// Alerts.
		r(get, "/alerts/rules", read, s.listRules),
		r(post, "/alerts/rules", alerts, s.createRule),
		r(post, "/alerts/rules/:test", alerts, s.testRule),
		r(get, "/alerts/rules/{id}", read, s.getRule),
		r(put, "/alerts/rules/{id}", alerts, s.putRule),
		r(del, "/alerts/rules/{id}", alerts, s.deleteRule),
		r(get, "/alerts/channels", members, s.listChannels),
		r(post, "/alerts/channels", members, s.createChannel),
		r(put, "/alerts/channels/{id}", members, s.putChannel),
		r(get, "/alerts/events", read, s.listAlertEvents),

		{method: get, pattern: "/.well-known/jwks.json", public: true, h: s.jwks},
		{method: get, pattern: "/v1/.well-known/jwks.json", public: true, h: s.jwks},
	}
	rs = append(rs, probe("/healthz", s.healthz)...)
	return append(rs, probe("/readyz", s.readyz)...)
}
