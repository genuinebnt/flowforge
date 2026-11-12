-- 0010_gateway_idempotency: HTTP Idempotency-Key replay store for the
-- gateway (section 12.1; 7-day retention as for SubmitRun keys, 9.10). The
-- gateway hosts the identity module, so the table lives in its schema.
--
-- A replay with the same request_hash returns status_code/response_body
-- with Idempotent-Replayed: true; the same key with a different hash is
-- 409 IDEMPOTENCY_KEY_REUSED. A row with completed_at NULL marks a request
-- in flight (a concurrent duplicate waits or gets 409). Rows past
-- expires_at are deleted by a periodic job.
CREATE TABLE identity.http_idempotency (
  tenant_id     uuid NOT NULL,
  key           text NOT NULL CHECK (char_length(key) BETWEEN 1 AND 128),
  method        text NOT NULL,
  path          text NOT NULL,
  request_hash  bytea NOT NULL,                         -- sha256 of method, path and canonical body
  status_code   integer,
  response_body bytea,
  content_type  text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  completed_at  timestamptz,
  expires_at    timestamptz NOT NULL DEFAULT now() + interval '7 days',
  PRIMARY KEY (tenant_id, key)
);
CREATE INDEX http_idempotency_expiry ON identity.http_idempotency (expires_at);

ALTER TABLE identity.http_idempotency ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON identity.http_idempotency
  USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
