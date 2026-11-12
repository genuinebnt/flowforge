package auth_test

import (
	"slices"
	"testing"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/testkit"
)

type rbacMatrix struct {
	Roles   []auth.Role `json:"roles"`
	Classes []struct {
		Permission  auth.Permission    `json:"permission"`
		Description string             `json:"description"`
		Allowed     map[auth.Role]bool `json:"allowed"`
	} `json:"classes"`
}

func TestAuth004_RBACMatrix(t *testing.T) {
	for _, f := range testkit.LoadCases[rbacMatrix](t, "auth/rbac") {
		m := f.Data
		if !slices.Equal(m.Roles, auth.Roles()) {
			t.Fatalf("%s: roles = %v, want %v", f.Name, m.Roles, auth.Roles())
		}
		var perms []auth.Permission
		for _, c := range m.Classes {
			perms = append(perms, c.Permission)
			for _, r := range m.Roles {
				want, ok := c.Allowed[r]
				if !ok {
					t.Fatalf("%s: class %s has no entry for %s", f.Name, c.Permission, r)
				}
				if got := auth.Can(r, c.Permission); got != want {
					t.Errorf("Can(%s, %s) = %v, want %v (%s)", r, c.Permission, got, want, c.Description)
				}
			}
		}
		if !slices.Equal(perms, auth.Permissions()) {
			t.Errorf("%s: permissions = %v, want %v", f.Name, perms, auth.Permissions())
		}
	}
	for _, p := range auth.Permissions() {
		if auth.Can("root", p) || auth.Can("", p) {
			t.Errorf("unknown role holds %s", p)
		}
	}
	if auth.Can(auth.RoleOwner, "billing:admin") {
		t.Error("owner holds an unknown permission")
	}
}

func TestApprovalRoleMatching(t *testing.T) {
	tests := []struct {
		name         string
		membership   auth.Role
		approvalRole string
		extra        []string
		want         bool
	}{
		{"approver addressed by membership role", auth.RoleApprover, "approver", nil, true},
		{"approver with matching group", auth.RoleApprover, "finance_ap", []string{"finance_ap"}, true},
		{"approver without the group", auth.RoleApprover, "finance_ap", []string{"lead"}, false},
		{"admin addressed as admin", auth.RoleAdmin, "admin", nil, true},
		{"owner does not match an approver approval", auth.RoleOwner, "approver", nil, false},
		{"analyst holds the group but not approvals:decide", auth.RoleAnalyst, "lead", []string{"lead"}, false},
		{"case-sensitive", auth.RoleApprover, "Finance_AP", []string{"finance_ap"}, false},
		{"empty approval role", auth.RoleApprover, "", []string{""}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := auth.CanDecideApproval(tt.membership, tt.approvalRole, tt.extra); got != tt.want {
				t.Fatalf("CanDecideApproval = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseRole(t *testing.T) {
	for _, r := range auth.Roles() {
		if got, err := auth.ParseRole(string(r)); err != nil || got != r {
			t.Errorf("ParseRole(%q) = %q, %v", r, got, err)
		}
	}
	if _, err := auth.ParseRole("Owner"); err == nil {
		t.Error(`ParseRole("Owner") accepted a non-canonical name`)
	}
}
