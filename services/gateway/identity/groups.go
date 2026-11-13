package identity

import (
	"context"
	"errors"
	"slices"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
)

// Role-group audit/outbox events (G-LIM-05).
const (
	EventRoleGroupMemberAdded   = "role_group_member_added"
	EventRoleGroupMemberRemoved = "role_group_member_removed"
)

// AddRoleGroupMember puts a tenant member into role group name (spec 9.7
// free-form approval roles; G-LIM-05). The name must be auth.ValidGroupName
// (400), the user a member of the caller's tenant (404), and a user holds
// at most auth.MaxGroups groups (400), so every member's groups fit the
// internal token's bounded claim. The change applies to tokens minted
// after it: the gateway reads groups per call, so no session bump is
// needed.
func (s *Service) AddRoleGroupMember(ctx context.Context, a Actor, name string, user core.UUID) error {
	if !auth.ValidGroupName(name) {
		return errValidation("name: a role group is [a-z][a-z0-9_.-]{0,63} and not a membership role")
	}
	err := s.cfg.Store.WithTx(ctx, func(tx Store) error {
		cur, err := tx.UserRoleGroups(ctx, a.TenantID, user)
		if err != nil {
			return err
		}
		if slices.Contains(cur, name) {
			return nil
		}
		if len(cur) >= auth.MaxGroups {
			return errValidation("user already belongs to %d role groups (the maximum)", auth.MaxGroups)
		}
		if err := tx.AddRoleGroupMember(ctx, a.TenantID, name, user, s.now()); err != nil {
			if errors.Is(err, ErrNotFound) {
				return errNotFound()
			}
			return err
		}
		return s.record(ctx, tx, EventRoleGroupMemberAdded, a.TenantID, a.UserID, a.Client, map[string]string{
			"group": name, "member": core.PublicID(core.PrefixUser, user), "by": actorName(a),
		})
	})
	return passOrInternalOrNil(err)
}

// RemoveRoleGroupMember removes user from role group name; removing a
// non-member is 404.
func (s *Service) RemoveRoleGroupMember(ctx context.Context, a Actor, name string, user core.UUID) error {
	if !auth.ValidGroupName(name) {
		return errNotFound()
	}
	err := s.cfg.Store.WithTx(ctx, func(tx Store) error {
		ok, err := tx.RemoveRoleGroupMember(ctx, a.TenantID, name, user)
		if err != nil {
			return err
		}
		if !ok {
			return errNotFound()
		}
		return s.record(ctx, tx, EventRoleGroupMemberRemoved, a.TenantID, a.UserID, a.Client, map[string]string{
			"group": name, "member": core.PublicID(core.PrefixUser, user), "by": actorName(a),
		})
	})
	return passOrInternalOrNil(err)
}

// ListRoleGroups lists the tenant's role groups with their members.
func (s *Service) ListRoleGroups(ctx context.Context, tenant core.UUID) ([]RoleGroup, error) {
	gs, err := s.cfg.Store.ListRoleGroups(ctx, tenant)
	if err != nil {
		return nil, errInternal(err)
	}
	return gs, nil
}

// UserRoleGroups returns the groups the gateway puts in a user's internal
// tokens. Names that no longer pass auth.ValidGroupName (rows written
// around the application) are dropped and the list is capped at
// auth.MaxGroups, so a stored row can never make the user's tokens
// unmintable; dropping a group only ever removes approval rights.
func (s *Service) UserRoleGroups(ctx context.Context, tenant, user core.UUID) ([]string, error) {
	gs, err := s.cfg.Store.UserRoleGroups(ctx, tenant, user)
	if err != nil {
		return nil, errInternal(err)
	}
	gs = slices.DeleteFunc(gs, func(g string) bool { return !auth.ValidGroupName(g) })
	if len(gs) > auth.MaxGroups {
		gs = gs[:auth.MaxGroups]
	}
	return gs, nil
}
