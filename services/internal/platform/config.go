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

	JWKSURL     string // FF_JWKS_URL: https://..., http://... or file:///path
	JWTIssuer   string // FF_JWT_ISSUER
	JWTAudience string // FF_JWT_AUDIENCE (flowforge-internal for internal services)
	// SigningKID and SigningSeed are this service's internal-token signing
	// key (FF_SIGNING_KID, FF_SIGNING_KEY: base64 or hex of a 32-byte
	// Ed25519 seed, mounted from Secret Manager).
	SigningKID  string
	SigningSeed []byte

	PubSubProject string // FF_PUBSUB_PROJECT (no adapter yet; see services/README.md)
	TopicPrefix   string // FF_TOPIC_PREFIX

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
		PubSubProject: getenv("FF_PUBSUB_PROJECT"), TopicPrefix: getenv("FF_TOPIC_PREFIX"),
		EgressProxy: getenv("FF_EGRESS_PROXY"),
		ScopeAddr:   getenv("FF_SCOPE_ADDR"), EngineAddr: getenv("FF_ENGINE_ADDR"), GraphAddr: getenv("FF_GRAPH_ADDR"),
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
	if s := getenv("FF_SIGNING_KEY"); s != "" {
		seed, err := decodeSeed(s)
		if err != nil {
			return c, fmt.Errorf("FF_SIGNING_KEY: %w", err)
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

func decodeSeed(s string) ([]byte, error) {
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
