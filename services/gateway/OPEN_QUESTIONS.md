# services/gateway: open questions

Choices made where spec sections 12-14 are silent or ambiguous. Each is
pinned by a test or a conformance transcript (`conformance/api`).

1. **Permissions of endpoints not in 13.6.** `POST /programs/{id}/scope:check`
   uses `workflows:write` (a "check", like `workflows:check`);
   `POST /graph/analysis` uses `runs:submit` (tenant compute); `GET
   /programs/{pid}/similarity/index` uses `programs:admin`; channels
   (including GET) use `members:admin` (configs may hold webhook targets);
   `GET /me` and `POST /auth/logout` need only a valid credential. As in
   `auth/OPEN_QUESTIONS.md` 14, 13.6 wins over the "Min. role" column.
2. **Approval role match.** The gateway applies `auth.CanDecideApproval`
   when the approval names one of the five membership roles; free-form
   approval roles (`lead`, `finance_ap`) depend on groups the engine owns,
   so the gateway only requires `approvals:decide` and leaves the match
   (and the self-approval rule) to the engine.
3. **Login failure reason.** 11.8 has no credentials reason; bad
   credentials, unknown e-mails and missing memberships are `401
   token_invalid` with one fixed message. "Email address not verified" is
   only returned after the password verified (reveals nothing).
4. **Login tenant.** Without `tenant_id` the oldest membership is used; a
   `tenant_id` the user is not a member of is the uniform 401.
5. **Signup for an existing e-mail** returns fresh random ids that were
   never stored, so the 202 body has the same shape either way.
6. **Password reset tokens** are stored in `identity.email_verifications`
   (no dedicated table) with a domain-separated hash
   (`sha256("password_reset\0" + token)`), so a reset token can never verify
   an address and vice versa. TTL 1 hour. A reset also verifies the
   address, revokes every session and bumps the session version.
7. **Invites** need an acceptance endpoint the spec does not list:
   `POST /auth/invites:accept {token, password}`. Unknown addresses become
   active users (the token proves the address); existing users gain the
   membership. Invite ids use the prefix `inv_`.
8. **Owner role.** Granting or removing `owner` needs `tenant:owner`; the
   last owner cannot be demoted (`400 validation_failed`); API keys cannot
   hold `owner`. A role change bumps the member's session version.
9. **CSRF and If-Match failures** have no 11.8 reason: gateway-local
   `CSRF_FAILED` (403) and `PRECONDITION_FAILED` (412). `FAILED_PRECONDITION`
   from services maps to 409 (11.8 says "409 / 412"); 412 is reserved for
   `If-Match`.
10. **ETags** are a hash of the resource's JSON form, because the program,
    rule and channel contracts carry no version; the If-Match check is a
    read-then-write (not atomic across the gateway and the service).
11. **Idempotency scope.** Keys are per tenant; the request hash covers
    method, path and canonical body, so the same key on another endpoint is
    `409`. A concurrent duplicate gets `409` (not a wait); an in-flight row
    older than 60 s is taken over. 4xx responses are stored and replayed,
    5xx are not. Replayed bodies keep the original `request_id`. Public
    `/auth/*` POSTs never store responses.
12. **Rate-limit keys for anonymous callers** are `rl:api:ip/<ip>:<class>`
    (14.3 only names tenant keys). Limiter errors fail open for API limits
    (auth throttles inside the identity service fail closed). Health,
    readiness and JWKS are not limited and carry no RateLimit headers.
    `RateLimit-Reset` is the time until the bucket is full again.
13. **Auth throttle keys** not named by 14.3: resend
    `rl:auth:acct:<sha256>:resend`, forgot `rl:auth:ip:<ip>:password_forgot`
    and `rl:auth:acct:<sha256>:forgot`. GCRA with burst = count and rate =
    count/period.
14. **Ids** are validated by prefix and character set only (`prg_…`,
    `run_…`; 1-128 of `[A-Za-z0-9_-]`) and passed to services unchanged; a
    wrong prefix in a path is 404, in a body or query 400.
15. **Route aliases.** 12.3 lists `/graph/entities/{id}/similar`, `POST
    /graph/similar`, `/graph/analysis`, `/graph/snapshots`; 22.9 lists the
    `/programs/{pid}/…` forms. Both are served (the 12.3 forms take
    `program_id` from the query/body, or from the entity). `GET
    /graph/snapshots` returns the one `SnapshotInfo` the contract offers as a
    one-item list. Similarity hits gain `type` (asset type) by a best-effort
    graph lookup.
16. **Unknown routes and methods** are `404 not_found` (11.8 has no
    method-not-allowed reason).
17. **Revocation lookups** that fail are `503 unavailable` (auth fails
    closed, `ErrRevocationUnavailable`).
18. **Internal token subject** is the bare `usr_…` or `key_…` id; `x-actor`
    carries `user:` / `apikey:` (11.1).
19. **Write deadlines** use the wall clock (socket deadlines), not the
    injected clock; SSE streams extend the deadline on every write.
