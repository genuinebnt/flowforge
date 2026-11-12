# auth: open questions

Choices made where spec section 13 is silent or ambiguous. Each is pinned
by a test or conformance file.

1. **Skew boundaries.** "30 seconds of clock skew": a token is expired when
   `now >= exp + 30s`; `iat` and `nbf` may be at most 30 s in the future.
   Pinned by `TestVerifierTimeBoundaries` and `accept_expired_within_skew` /
   `expired` in `conformance/auth/jwt`.
2. **`iat` in the future vs `nbf`.** auth_002 asks for "not yet valid".
   `nbf` in the future is `not_yet_valid`; `iat` in the future is a distinct
   `iat_future` rejection.
3. **Required claims.** `iss`, `aud`, `sub`, `tid`, `role`, `jti`, `iat`,
   `exp` are required; `sid` and `sv` are optional (internal tokens have no
   session). `role` must be one of the five roles. `aud` must be a single
   string (arrays are rejected as malformed; Flowforge never issues them).
4. **Header strictness.** `typ` must be exactly `JWT`; `crit` is rejected;
   `jku`/`jwk`/`x5u` are ignored (keys come only from the key set).
   Duplicate member names, case variants of known names (`ALG`) and
   non-canonical base64url (padding, stray bits) are malformed, so no two
   parsers can read one token differently. `alg` is compared exactly
   (`eddsa` is rejected); any case of `none` is reported as `alg_none`.
5. **Retired keys** are rejected with their own reason (`key_retired`)
   rather than as unknown kids, and are not published in the JWKS.
   Parsed JWKS keys are treated as verifying keys (state `active`).
6. **Key refresh.** The first load counts as the minute's refresh for
   unknown kids. The cached set is also refetched after 300 s (the JWKS
   max-age) so retirements propagate; a failed refetch keeps the old set.
7. **Session version.** A missing `sv:<user>` cache entry means "not
   revoked": the entry outlives every access token issued before a bump.
   A token without `sv` (0) is revoked once any version is cached. A
   revocation lookup error rejects the token (`ErrRevocationUnavailable`,
   fail closed) but is not an `ErrInvalidToken`.
8. **Internal tokens** carry the actor in `sub`, use no revocation lookup
   and are rejected when `exp - iat > 60 s`.
9. **Refresh expiry.** 13.4 rejects `expires_at < now`, so a token is still
   valid at exactly `expires_at`. A failed compare-and-set on `used_at`
   (concurrent presentation) is treated as reuse.
10. **API key ids** are 1-64 ASCII letters or digits, because the
    base64url secret may contain `_` and the first `_` after the id must
    end it. Suggested id: the Crockford body of the key's UUIDv7.
11. **Lockout.** "After 10 failures for an email, require a 15-minute
    cool-down": failures count since the last success or cool-down (no
    sliding window); the 10th failure starts the cool-down; during it even
    a correct password is refused with the same throttled response. The
    lockout key is the account throttle key (SHA-256 of the normalized
    email), so unknown emails lock out identically.
12. **Pending and disabled users** are refused only after the password
    verifies (distinct errors then). Checking status first would reveal
    which emails exist.
13. **Approval roles** (9.7) are free-form names (`finance_ap`, `lead`).
    `CanDecideApproval` requires `approvals:decide` and an exact,
    case-sensitive match against the membership role or an extra group the
    caller supplies; owners and admins are not wildcards. The self-approval
    rule is left to the engine.
14. **12.3 "Min. role" vs 13.6.** The endpoint table reads like a
    hierarchy, but 13.6 gives approvers no analyst permissions. 13.6 wins.
15. **Breached-password checker failure** rejects the password (fail
    closed).
16. **Hash cost.** b_auth_01 asks for 200-500 ms per hash on target
    hardware; the 13.1 floor (19 MiB, t=2, p=1) measures about 20-25 ms on
    the development container. `PolicyParams` stays at the floor; production
    should raise `Time` or `Memory` after measuring on the target machines.
    Existing hashes upgrade on the next login through `NeedsRehash`.
