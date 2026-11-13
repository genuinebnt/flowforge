# services/gateway/identity

The identity module hosted by the gateway (spec 10.2, 13, 14.3): signup,
e-mail verification, login with throttles and lockout, refresh rotation
with reuse detection, logout, password reset, members and invites, API
keys, signing-key rotation, `auth_audit` and the `auth.events` outbox.
It composes package `auth`; see `../README.md` for the flows.

| Port | In-memory | Production |
|------|-----------|------------|
| `Store` (identity schema) | `MemStore` | `pgstore.Store` (pgx) |
| `Revocations` (deny:jti, sv) | `MemRevocations` | `redisstore.Revocations` |
| `auth.LockoutStore` | `auth.InMemoryLockoutStore` | `redisstore.Lockouts` |
| `LimiterFactory` (rl:auth:*) | `MemLimiterFactory` (GCRA) | `redislimiter` |
| `KeyStore` (private keys) | `MemKeyStore` | `keystore.Provisioned` (`FromEnv`: Terraform's `FF_SIGNING_KEY_<KID>`; `FromSecretManager`), `keystore.Dir`, `keystore.SecretManager`. A `ProvisionedKeys` store makes rotation operator-driven (`ErrOperatorRotation`) |
| `EmailSender` | `MemEmailSender`, `LogEmailSender` | provider adapter (not implemented) |

Role groups (`groups.go`, migration 0015) back free-form approval roles:
`AddRoleGroupMember`, `RemoveRoleGroupMember`, `ListRoleGroups`,
`UserRoleGroups` (the names the gateway puts in the internal token).

`identitytest` builds a fully wired in-memory service and holds the Store
contract suite run against both stores.

```sh
go test ./services/gateway/identity/...
FF_TEST_PG_DSN=postgres://... go test ./services/gateway/identity/pgstore/
FF_TEST_REDIS_ADDR=127.0.0.1:6379 go test ./services/gateway/identity/redisstore/
```
