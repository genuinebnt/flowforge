// Package auth implements Flowforge authentication and authorization
// primitives (spec section 13, with the identity tables of 10.2 and the
// auth limiter keys of 14.3):
//
//   - Passwords (13.1): Argon2id hashing in PHC string format with
//     constant-time verification and re-hash detection (Hasher), and the
//     length/breached-list policy (PasswordPolicy, BreachedChecker).
//   - Access and internal tokens (13.2, 13.3, 13.7): JWS compact EdDSA
//     (Signer, Minter, Verifier) with a key set of active, retiring and
//     retired Ed25519 keys published as a JWKS (KeySet, KeyCache), and
//     revocation by jti deny-list and session version (RevocationChecker).
//   - Opaque secrets (13.1, 13.2): refresh tokens (ffr_), API keys
//     (ffk_<id>_<secret>), email verification and CSRF tokens. Only their
//     SHA-256 is stored.
//   - Refresh rotation with reuse detection (13.4) over a RefreshStore
//     (Refresher).
//   - Login (13.1): uniform responses for unknown and known emails, the
//     per-IP and per-email throttles and the 10-failure cool-down
//     (LoginService).
//   - Roles and permissions (13.6): Can and the approval-role check of 9.7.
//
// The package performs no I/O itself: storage, rate limiting, breached
// password lookups and key distribution are ports with in-memory fakes.
// Secrets are drawn from an injected io.Reader (crypto/rand.Reader in
// production), never from core.Rng; identifiers, which need uniqueness but
// not secrecy, come from core.Rng. Time comes from core.Clock.
//
// Every rejection has its own sentinel error so services can log the exact
// reason while answering clients with a uniform 401 (spec 11.8 mapping is
// the service's job): token rejections match ErrInvalidToken, refresh
// rejections match ErrInvalidRefresh, and RejectionReason returns a stable
// code for either.
package auth
