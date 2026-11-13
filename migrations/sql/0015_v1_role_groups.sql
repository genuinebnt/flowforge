-- 0015_v1_role_groups: tenant role groups (G-LIM-05).
--
-- An approval names a role (spec 9.7). The five membership roles of 13.6
-- cover only part of what workflows ask for (`finance_ap`, `lead`), so a
-- tenant admin groups members under free-form names. The gateway puts the
-- caller's group names in the internal token (`groups` claim) and the
-- engine matches approval.role against the membership role or a group.
--
-- * role_groups: one row per (tenant, name); created implicitly when the
--   first member is added and deleted with its last member, so a group
--   never exists empty.
-- * role_group_members: one row per (tenant, name, user); the user must be
--   a member of the tenant (FK to memberships), so removing the membership
--   removes the user from every group.
--
-- Names are lower case [a-z][a-z0-9_.-]{0,63} and never a membership role
-- name: a group called "admin" would let a viewer match approvals meant for
-- admins (the application validates the same rule; the CHECK is the
-- backstop).
CREATE TABLE identity.role_groups (
  tenant_id  uuid NOT NULL REFERENCES identity.tenants(id) ON DELETE CASCADE,
  name       text NOT NULL CHECK (name ~ '^[a-z][a-z0-9_.-]{0,63}$'
                                  AND name NOT IN ('owner','admin','approver','analyst','viewer')),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, name)
);

CREATE TABLE identity.role_group_members (
  tenant_id  uuid NOT NULL,
  name       text NOT NULL,
  user_id    uuid NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, name, user_id),
  FOREIGN KEY (tenant_id, name) REFERENCES identity.role_groups(tenant_id, name) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, user_id) REFERENCES identity.memberships(tenant_id, user_id) ON DELETE CASCADE
);

-- The gateway reads a caller's groups by (tenant, user).
CREATE INDEX role_group_members_user ON identity.role_group_members (tenant_id, user_id);

-- Same defense in depth as 0007 for tables with tenant_id.
ALTER TABLE identity.role_groups ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON identity.role_groups
  USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
ALTER TABLE identity.role_group_members ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON identity.role_group_members
  USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
