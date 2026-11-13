package platform

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
)

// Config is the environment of one service binary (spec 17.5). Only cmd
// mains read the environment (ConfigFromEnv); everything below them gets
// this struct.
type Config struct {
	Service    string
	Env        string // FF_ENV: dev or prod
	ListenAddr string // FF_LISTEN_ADDR, else ":" + PORT (default 8080)
	LogLevel   string // FF_LOG_LEVEL

	DBDSN     string // FF_DB_DSN
	RedisAddr string // FF_REDIS_ADDR
	RedisAuth string // FF_REDIS_AUTH
	// RedisCA is FF_REDIS_CA: the PEM of the CA that signed the Redis
	// server certificate (Memorystore's server CA, from the ff-redis-ca
	// secret) or a path to a PEM file. Set means the client uses TLS.
	RedisCA string
	// SecretProject is the default project of short sm://NAME secret
	// references (FF_SECRET_PROJECT, else FF_PUBSUB_PROJECT).
	SecretProject string
	// IDTokens: attach a Google ID token to calls to https:// service
	// addresses (Cloud Run IAM, spec 17.3; G-SVC-13). True when
	// FF_ID_TOKEN_AUDIENCE_AUTO is true or the process runs on Cloud Run
	// (K_SERVICE, set by the platform); FF_ID_TOKEN_AUDIENCE_AUTO=false
	// turns it off there.
	IDTokens bool

	JWKSURL     string // FF_JWKS_URL: https://..., http://... or file:///path
	JWTIssuer   string // FF_JWT_ISSUER
	JWTAudience string // FF_JWT_AUDIENCE (flowforge-internal for internal services)
	// SigningKID and SigningSeed are the internal-token signing key:
	// FF_SIGNING_KID and the seed in FF_SIGNING_KEY_<KID> (SigningKeyVar,
	// the name Terraform mounts it under), else FF_SIGNING_KEY. It is the
	// gateway's provisioned key, which the gateway registers and publishes
	// in its JWKS, so tokens a service mints with it verify at every
	// service that trusts FF_JWKS_URL (G-OPS-12).
	SigningKID  string
	SigningSeed []byte

	// Bus is FF_BUS: mem, postgres or pubsub. Empty picks pubsub when
	// PubSubProject is set, postgres when DBDSN is set, else mem
	// (busconfig.Resolve).
	Bus           string
	PubSubProject string // FF_PUBSUB_PROJECT
	TopicPrefix   string // FF_TOPIC_PREFIX
	// PubSubCreate (FF_PUBSUB_CREATE) lets the Pub/Sub adapter create
	// missing topics and subscriptions (emulator, lab without Terraform).
	PubSubCreate bool

	// Blob storage for raw tool output, snapshot files and run archives:
	// FF_ARTIFACT_BUCKET selects Cloud Storage, else FF_BLOB_DIR a local
	// directory, else memory (services/internal/blob).
	ArtifactBucket string
	BlobDir        string

	// TickAudience and TickServiceAccount (FF_TICK_OIDC_AUDIENCE,
	// FF_TICK_SERVICE_ACCOUNT) let POST /internal/schedules:tick accept
	// the Google ID token Cloud Scheduler sends, besides internal tokens.
	TickAudience, TickServiceAccount string
	// MaintenanceInterval (FF_MAINTENANCE_INTERVAL, a Go duration; "0"
	// disables) is how often the engine process runs the retention and
	// partition jobs in process (default: hourly with Postgres).
	MaintenanceInterval string

	EgressProxy string // FF_EGRESS_PROXY

	ScopeAddr      string // FF_SCOPE_ADDR
	EngineAddr     string // FF_ENGINE_ADDR
	GraphAddr      string // FF_GRAPH_ADDR
	ToolRunnerAddr string // FF_TOOL_RUNNER_ADDR
	NotifierAddr   string // FF_NOTIFIER_ADDR
	SimilarityAddr string // FF_SIMILARITY_ADDR

	FuelDefault int64 // FF_FUEL_DEFAULT
	Workers     int   // FF_ENGINE_WORKERS
}

// ConfigFromEnv reads the variables of spec 17.5 through getenv
// (os.Getenv in a main).
func ConfigFromEnv(service string, getenv func(string) string) (Config, error) {
	c := Config{
		Service: service, Env: or(getenv("FF_ENV"), "dev"), LogLevel: getenv("FF_LOG_LEVEL"),
		DBDSN: getenv("FF_DB_DSN"), RedisAddr: getenv("FF_REDIS_ADDR"), RedisAuth: getenv("FF_REDIS_AUTH"),
		JWKSURL: getenv("FF_JWKS_URL"), JWTIssuer: or(getenv("FF_JWT_ISSUER"), "flowforge"),
		JWTAudience: or(getenv("FF_JWT_AUDIENCE"), auth.AudienceInternal), SigningKID: getenv("FF_SIGNING_KID"),
		Bus: getenv("FF_BUS"), PubSubProject: getenv("FF_PUBSUB_PROJECT"), TopicPrefix: getenv("FF_TOPIC_PREFIX"),
		PubSubCreate:   boolEnv(getenv("FF_PUBSUB_CREATE")),
		ArtifactBucket: getenv("FF_ARTIFACT_BUCKET"), BlobDir: getenv("FF_BLOB_DIR"),
		TickAudience: getenv("FF_TICK_OIDC_AUDIENCE"), TickServiceAccount: getenv("FF_TICK_SERVICE_ACCOUNT"),
		MaintenanceInterval: getenv("FF_MAINTENANCE_INTERVAL"),
		EgressProxy:         getenv("FF_EGRESS_PROXY"),
		ScopeAddr:           getenv("FF_SCOPE_ADDR"), EngineAddr: getenv("FF_ENGINE_ADDR"), GraphAddr: getenv("FF_GRAPH_ADDR"),
		ToolRunnerAddr: getenv("FF_TOOL_RUNNER_ADDR"), NotifierAddr: getenv("FF_NOTIFIER_ADDR"),
		SimilarityAddr: getenv("FF_SIMILARITY_ADDR"),
	}
	c.ListenAddr = getenv("FF_LISTEN_ADDR")
	if c.ListenAddr == "" {
		c.ListenAddr = ":" + or(getenv("PORT"), "8080")
	}
	if c.Env != "dev" && c.Env != "prod" {
		return c, fmt.Errorf("FF_ENV must be dev or prod, got %q", c.Env)
	}
	c.RedisCA = getenv("FF_REDIS_CA")
	c.SecretProject = or(getenv("FF_SECRET_PROJECT"), c.PubSubProject)
	c.IDTokens = IDTokensFromEnv(getenv)
	if name, v := SigningKeyEnv(c.SigningKID, getenv); v != "" {
		seed, err := DecodeSeed(v)
		if err != nil {
			return c, fmt.Errorf("%s: %w", name, err)
		}
		c.SigningSeed = seed
	}
	var err error
	if c.FuelDefault, err = intEnv(getenv, "FF_FUEL_DEFAULT"); err != nil {
		return c, err
	}
	w, err := intEnv(getenv, "FF_ENGINE_WORKERS")
	if err != nil {
		return c, err
	}
	c.Workers = int(w)
	return c, nil
}

func boolEnv(v string) bool {
	b, _ := strconv.ParseBool(v)
	return b
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func intEnv(getenv func(string) string, k string) (int64, error) {
	s := getenv(k)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return n, nil
}

// IDTokensFromEnv reports whether calls to https:// services carry a
// Google ID token (Config.IDTokens).
func IDTokensFromEnv(getenv func(string) string) bool {
	if v := getenv("FF_ID_TOKEN_AUDIENCE_AUTO"); v != "" {
		b, _ := strconv.ParseBool(v)
		return b
	}
	return getenv("K_SERVICE") != ""
}

// SigningKeyVar is the environment variable Terraform mounts the secret
// ff-signing-key-<kid> under: FF_SIGNING_KEY_ plus the kid upper-cased with
// '-' as '_' (infra/envs/*/main.tf signing_key_env). The mapping is not
// injective (a-b and a_b collide); kids are dates plus a suffix, so that
// is accepted.
func SigningKeyVar(kid string) string {
	return "FF_SIGNING_KEY_" + strings.ToUpper(strings.ReplaceAll(kid, "-", "_"))
}

// SigningKeyEnv returns the variable holding the signing seed of kid and
// its value: FF_SIGNING_KEY_<KID> when set, else the single-key
// FF_SIGNING_KEY kept for compatibility (G-OPS-12).
func SigningKeyEnv(kid string, getenv func(string) string) (name, value string) {
	if kid != "" {
		if v := getenv(SigningKeyVar(kid)); v != "" {
			return SigningKeyVar(kid), v
		}
	}
	return "FF_SIGNING_KEY", getenv("FF_SIGNING_KEY")
}

// DecodeSeed decodes a 32-byte Ed25519 seed written as hex, standard or
// URL-safe base64, or (a binary Secret Manager payload) the raw 32 bytes.
// The forms cannot be confused: hex is 64 characters, base64 43 or 44.
func DecodeSeed(s string) ([]byte, error) {
	if len(s) == 32 {
		return []byte(s), nil
	}
	s = strings.TrimSpace(s)
	if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	return nil, fmt.Errorf("want 32 bytes in hex or base64")
}

// LoadKeys builds the internal-token keys of c: the signing key from
// SigningSeed and the verification keys from JWKSURL (cached, refreshed by
// auth.KeyCache). In dev without any key material an ephemeral key is
// generated, which only works when every service runs in one process
// (flowforge dev).
func LoadKeys(ctx context.Context, c Config, clock core.Clock) (Keys, error) {
	k := Keys{Issuer: c.JWTIssuer}
	var signing *auth.SigningKey
	if len(c.SigningSeed) > 0 {
		key, err := auth.NewSigningKey(or(c.SigningKID, c.Service), c.SigningSeed, auth.KeyActive)
		if err != nil {
			return k, err
		}
		s, err := auth.NewSigner(key)
		if err != nil {
			return k, err
		}
		k.Signer, signing = s, &key
	}
	switch {
	case c.JWKSURL != "":
		src := auth.KeySourceFunc(func(ctx context.Context) (*auth.KeySet, error) { return fetchJWKS(ctx, c.JWKSURL) })
		cache, err := auth.NewKeyCache(auth.KeyCacheConfig{Source: src, Clock: clock})
		if err != nil {
			return k, err
		}
		k.Resolver = cache
	case signing != nil:
		set, err := auth.NewKeySet(signing.Public())
		if err != nil {
			return k, err
		}
		k.Resolver = set
	case c.Env == "dev":
		return NewDevKeys(c.JWTIssuer)
	default:
		return k, fmt.Errorf("FF_JWKS_URL is required in %s", c.Env)
	}
	return k, nil
}

func fetchJWKS(ctx context.Context, raw string) (*auth.KeySet, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	var body []byte
	if u.Scheme == "file" {
		body, err = os.ReadFile(u.Path)
	} else {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if rerr != nil {
			return nil, rerr
		}
		resp, rerr := http.DefaultClient.Do(req)
		if rerr != nil {
			return nil, rerr
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("jwks: HTTP %d", resp.StatusCode)
		}
		body, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	}
	if err != nil {
		return nil, err
	}
	return auth.ParseJWKS(body)
}
