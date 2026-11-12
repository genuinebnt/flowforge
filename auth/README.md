# auth

Authentication and authorization primitives for Flowforge: password
hashing, EdDSA access/internal tokens, key sets, opaque secrets, refresh
rotation with reuse detection, login throttling and RBAC.

Spec: section 13 (13.1 signup and verification, 13.2 tokens, 13.3
validation, 13.4 refresh rotation, 13.5 CSRF, 13.6 roles, 13.7 key
rotation), identity tables 10.2, limiter keys 14.3, tests auth_001..005,
fuzz_jwt, benchmark b_auth_01.

## API at a glance

| Area | Identifiers |
|---|---|
| Passwords | `Hasher` (`NewHasher`, `Hash`, `Verify`, `NeedsRehash`), `Params`, `PolicyParams`, `PasswordPolicy`, `BreachedChecker`, `BreachedSet` |
| Keys | `SigningKey`, `PublicKey`, `KeyState`, `KeySet` (JWKS `MarshalJSON`, `ParseJWKS`), `KeyResolver`, `KeyCache`, `KeySource`, `MemoryKeySource` |
| Tokens | `Claims`, `Signer`, `Minter` (`Access`, `Internal`), `Verifier`, `VerifierConfig`, `InternalVerifierConfig`, `RevocationChecker`, `InMemoryRevocation`, `NoRevocation` |
| Opaque secrets | `NewRefreshToken`, `HashToken`, `NewAPIKey`, `ParseAPIKey`, `EmailVerifier`, `VerificationStore`, `NewCSRFToken`, `CheckCSRF`, `CSRFRequired` |
| Refresh rotation | `Refresher` (`Start`, `Rotate`, `RevokeSession`), `RefreshStore`, `InMemoryRefreshStore` |
| Login | `LoginService`, `UserStore`, `Throttle`, `LockoutStore`, `PasswordHasher` and in-memory fakes; `IPThrottleKey`, `AccountThrottleKey` |
| RBAC | `Role`, `Permission`, `Can`, `PermissionsOf`, `ApprovalRoleMatches`, `CanDecideApproval` |
| Errors | one sentinel per rejection; `RejectionReason(err)` gives the stable code; `ErrInvalidToken` / `ErrInvalidRefresh` match every token / refresh rejection |

Secrets (salts, refresh tokens, API keys, verification and CSRF tokens,
key seeds) come from an injected `io.Reader`: pass `crypto/rand.Reader` in
production. Identifiers (jti, row ids) come from a `core.Rng`.

## Tests and benchmarks

```
go test ./auth/...
go test -run '^$' -fuzz FuzzJWT -fuzztime 30s ./auth/
go test -run '^$' -bench Auth01 ./auth/
```

Conformance data: `conformance/auth/jwt/*.json` (one self-contained
verification vector per file: key seeds and states, `now`, issuer,
audience, revocation state, token, expected accept or reason) and
`conformance/auth/rbac/matrix.json` (13.6). Unit tests use reduced Argon2id
parameters through `AllowWeakParams`; `TestAuth001_Argon2/policy_params`
and `BenchmarkAuth01_Hash` use the real 13.1 parameters.

## Limitations

* No signup flow, invites, password reset or Google ID token / mTLS
  alternatives (17); those are service compositions of these primitives.
* `KeyCache` serializes fetches under its lock; a slow key source delays
  verifications that need a refresh (at most once per minute).
* Rehash-on-login failures are swallowed (retried next login); the result
  reports `Rehashed`.
* Password length counts Unicode code points without normalization (no
  `golang.org/x/text` dependency in layer 1).
