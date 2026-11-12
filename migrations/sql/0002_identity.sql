-- 0002_identity: spec section 10.2, transcribed verbatim.

CREATE SCHEMA identity;

CREATE TABLE identity.tenants (
  id          uuid PRIMARY KEY,
  name        text NOT NULL,
  slug        citext NOT NULL UNIQUE,
  plan        text NOT NULL DEFAULT 'free',
  created_at  timestamptz NOT NULL DEFAULT now(),
  disabled_at timestamptz
);

CREATE TABLE identity.users (
  id                uuid PRIMARY KEY,
  email             citext NOT NULL UNIQUE,
  email_verified_at timestamptz,
  password_hash     text NOT NULL,                  -- argon2id PHC string
  status            text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','active','disabled')),
  created_at        timestamptz NOT NULL DEFAULT now(),
  last_login_at     timestamptz,
  session_version   integer NOT NULL DEFAULT 1      -- bump to invalidate all access tokens
);

CREATE TABLE identity.memberships (
  tenant_id  uuid NOT NULL REFERENCES identity.tenants(id) ON DELETE CASCADE,
  user_id    uuid NOT NULL REFERENCES identity.users(id)   ON DELETE CASCADE,
  role       text NOT NULL CHECK (role IN ('owner','admin','approver','analyst','viewer')),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, user_id)
);

CREATE TABLE identity.invites (
  id uuid PRIMARY KEY, tenant_id uuid NOT NULL REFERENCES identity.tenants(id) ON DELETE CASCADE,
  email citext NOT NULL, role text NOT NULL, token_hash bytea NOT NULL UNIQUE,
  expires_at timestamptz NOT NULL, accepted_at timestamptz, created_by uuid NOT NULL, created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE identity.email_verifications (
  token_hash bytea PRIMARY KEY, user_id uuid NOT NULL REFERENCES identity.users(id) ON DELETE CASCADE,
  expires_at timestamptz NOT NULL, used_at timestamptz
);

CREATE TABLE identity.refresh_tokens (
  id          uuid PRIMARY KEY,
  family_id   uuid NOT NULL,                        -- one login session = one family
  user_id     uuid NOT NULL REFERENCES identity.users(id) ON DELETE CASCADE,
  tenant_id   uuid NOT NULL REFERENCES identity.tenants(id) ON DELETE CASCADE,
  token_hash  bytea NOT NULL UNIQUE,                -- sha256 of the opaque token
  parent_id   uuid REFERENCES identity.refresh_tokens(id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  expires_at  timestamptz NOT NULL,
  used_at     timestamptz,                          -- set on rotation; reuse of a used token revokes the family
  revoked_at  timestamptz,
  user_agent  text, ip inet
);
CREATE INDEX ON identity.refresh_tokens (family_id);

CREATE TABLE identity.api_keys (
  id uuid PRIMARY KEY, tenant_id uuid NOT NULL REFERENCES identity.tenants(id) ON DELETE CASCADE,
  name text NOT NULL, prefix text NOT NULL UNIQUE,  -- public part: ffk_<id>
  secret_hash bytea NOT NULL, role text NOT NULL, created_by uuid NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(), last_used_at timestamptz, expires_at timestamptz, revoked_at timestamptz
);

CREATE TABLE identity.signing_keys (
  kid text PRIMARY KEY, alg text NOT NULL DEFAULT 'EdDSA', public_jwk jsonb NOT NULL,
  private_key_ref text NOT NULL,                    -- Secret Manager resource name; never the key itself
  status text NOT NULL CHECK (status IN ('active','retiring','retired')),
  created_at timestamptz NOT NULL DEFAULT now(), retire_at timestamptz
);

CREATE TABLE identity.auth_audit (
  id bigserial PRIMARY KEY, at timestamptz NOT NULL DEFAULT now(), tenant_id uuid, user_id uuid,
  event text NOT NULL, ip inet, user_agent text, detail jsonb NOT NULL DEFAULT '{}'
);
