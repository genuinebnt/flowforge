package auth

import (
	"fmt"
	"slices"
)

// Role is a tenant membership role (identity.memberships.role).
type Role string

// Roles, from least to most privileged.
const (
	RoleViewer   Role = "viewer"
	RoleAnalyst  Role = "analyst"
	RoleApprover Role = "approver"
	RoleAdmin    Role = "admin"
	RoleOwner    Role = "owner"
)

// Roles returns every role, least privileged first.
func Roles() []Role {
	return []Role{RoleViewer, RoleAnalyst, RoleApprover, RoleAdmin, RoleOwner}
}

// Valid reports whether r is one of the five roles.
func (r Role) Valid() bool { return slices.Contains(Roles(), r) }

// ParseRole parses a role name.
func ParseRole(s string) (Role, error) {
	if r := Role(s); r.Valid() {
		return r, nil
	}
	return "", fmt.Errorf("auth: unknown role %q", s)
}

// Permission is one row of the spec 13.6 table.
type Permission string

// Permissions (spec 13.6).
const (
	// PermRead: read programs, runs, events, graph, alerts.
	PermRead Permission = "read"
	// PermWorkflowsWrite: check and register workflows.
	PermWorkflowsWrite Permission = "workflows:write"
	// PermRunsSubmit: submit and cancel runs (same table row as
	// PermWorkflowsWrite).
	PermRunsSubmit Permission = "runs:submit"
	// PermApprovalsDecide: decide approvals.
	PermApprovalsDecide Permission = "approvals:decide"
	// PermAlertsWrite: create and edit alert rules.
	PermAlertsWrite Permission = "alerts:write"
	// PermProgramsAdmin: edit programs and scope, use the kill switch.
	PermProgramsAdmin Permission = "programs:admin"
	// PermMembersAdmin: manage members, API keys and alert channels.
	PermMembersAdmin Permission = "members:admin"
	// PermTenantOwner: delete the tenant, transfer ownership.
	PermTenantOwner Permission = "tenant:owner"
)

// Permissions returns every permission in table order.
func Permissions() []Permission {
	return []Permission{
		PermRead, PermWorkflowsWrite, PermRunsSubmit, PermApprovalsDecide,
		PermAlertsWrite, PermProgramsAdmin, PermMembersAdmin, PermTenantOwner,
	}
}

// grants is the spec 13.6 table, as a function so it cannot be mutated.
func grants(r Role) []Permission {
	switch r {
	case RoleViewer:
		return []Permission{PermRead}
	case RoleAnalyst:
		return []Permission{PermRead, PermWorkflowsWrite, PermRunsSubmit, PermAlertsWrite}
	case RoleApprover:
		return []Permission{PermRead, PermApprovalsDecide}
	case RoleAdmin:
		return []Permission{PermRead, PermWorkflowsWrite, PermRunsSubmit, PermApprovalsDecide,
			PermAlertsWrite, PermProgramsAdmin, PermMembersAdmin}
	case RoleOwner:
		return Permissions()
	}
	return nil
}

// Can reports whether role r holds permission p. Unknown roles and unknown
// permissions hold nothing.
func Can(r Role, p Permission) bool { return slices.Contains(grants(r), p) }

// PermissionsOf returns the permissions of r in table order.
func PermissionsOf(r Role) []Permission { return slices.Clone(grants(r)) }

// ApprovalRoleMatches reports whether an approval addressed to approvalRole
// (spec 9.7) is addressed to the decider: approvalRole equals the
// decider's membership role or one of the extra approval groups they hold.
// The match is exact and case-sensitive; an empty approvalRole never
// matches.
func ApprovalRoleMatches(approvalRole string, membership Role, extra []string) bool {
	if approvalRole == "" {
		return false
	}
	return approvalRole == string(membership) || slices.Contains(extra, approvalRole)
}

// CanDecideApproval combines the two conditions of spec 9.7: the decider
// holds approvals:decide and the approval's role matches a role they hold.
// The self-approval rule is the engine's, not checked here.
func CanDecideApproval(membership Role, approvalRole string, extra []string) bool {
	return Can(membership, PermApprovalsDecide) && ApprovalRoleMatches(approvalRole, membership, extra)
}
