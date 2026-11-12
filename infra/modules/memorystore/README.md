# memorystore

Memorystore for Redis 7.2 (spec 17.1): `PRIVATE_SERVICE_ACCESS` on the
environment VPC, `auth_enabled = true`, `transit_encryption_mode =
SERVER_AUTHENTICATION` (TLS on port 6378), `BASIC` in dev and `STANDARD_HA`
in prod.

Outputs `address` (FF_REDIS_ADDR), the sensitive `auth_string` and the
`server_ca_pem`; the environment stores both in Secret Manager
(`ff-redis-auth`, `ff-redis-ca`) and mounts them as `FF_REDIS_AUTH` and
`FF_REDIS_CA` on gateway, engine, scope and tool-runner (spec 17.5).
