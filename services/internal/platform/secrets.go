package platform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Secrets resolves a secret reference to its value (spec 15.4 channel
// configs, 13.7 private_key_ref, 17.1 Secret Manager). References are
// URIs:
//
//   - env://NAME          the process environment (Secret Manager values
//     mounted as env vars on Cloud Run, or exported locally)
//   - file:///abs/path    a file (Secret Manager volume mounts, local key
//     files); surrounding whitespace is trimmed
//   - sm://projects/P/secrets/S/versions/V   Secret Manager (smsecrets)
//
// Values are returned to the caller only; implementations never log them.
// Implementations are safe for concurrent use.
type Secrets interface {
	Resolve(ctx context.Context, ref string) ([]byte, error)
}

// Secret resolution errors. Callers treat both as "no secret" and fail
// closed (a webhook without its signing secret is not sent).
var (
	// ErrSecretRef reports a reference whose scheme no resolver handles,
	// or that is malformed.
	ErrSecretRef = errors.New("platform: unsupported secret reference")
	// ErrSecretNotFound reports a well-formed reference with no value.
	ErrSecretNotFound = errors.New("platform: secret not found")
)

// MemSecrets is an in-memory Secrets keyed by the whole reference (tests,
// the dev stack).
type MemSecrets struct {
	mu sync.Mutex
	m  map[string][]byte
}

// NewMemSecrets returns a resolver over m.
func NewMemSecrets(m map[string]string) *MemSecrets {
	s := &MemSecrets{m: map[string][]byte{}}
	for k, v := range m {
		s.m[k] = []byte(v)
	}
	return s
}

// Set stores a secret under ref.
func (s *MemSecrets) Set(ref, value string) {
	s.mu.Lock()
	s.m[ref] = []byte(value)
	s.mu.Unlock()
}

// Resolve implements Secrets.
func (s *MemSecrets) Resolve(_ context.Context, ref string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.m[ref]; ok {
		return bytes.Clone(v), nil
	}
	return nil, fmt.Errorf("%w: %q", ErrSecretNotFound, ref)
}

// EnvSecrets resolves env://NAME through Getenv (os.Getenv in a main; only
// cmd reads the environment). An empty value is ErrSecretNotFound, since
// Getenv cannot tell unset from empty and an empty secret is never valid.
type EnvSecrets struct{ Getenv func(string) string }

// Resolve implements Secrets.
func (e EnvSecrets) Resolve(_ context.Context, ref string) ([]byte, error) {
	name, ok := strings.CutPrefix(ref, "env://")
	if !ok || name == "" || e.Getenv == nil {
		return nil, fmt.Errorf("%w: %q", ErrSecretRef, ref)
	}
	if v := e.Getenv(name); v != "" {
		return []byte(v), nil
	}
	return nil, fmt.Errorf("%w: %q", ErrSecretNotFound, ref)
}

// FileSecrets resolves file:///abs/path. When Root is set, the cleaned path
// must lie inside it, so a stored reference cannot read arbitrary files.
type FileSecrets struct{ Root string }

// Resolve implements Secrets.
func (f FileSecrets) Resolve(_ context.Context, ref string) ([]byte, error) {
	p, ok := strings.CutPrefix(ref, "file://")
	if !ok || !filepath.IsAbs(p) {
		return nil, fmt.Errorf("%w: %q (want file:///absolute/path)", ErrSecretRef, ref)
	}
	p = filepath.Clean(p)
	if f.Root != "" {
		rel, err := filepath.Rel(filepath.Clean(f.Root), p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("%w: %q is outside %s", ErrSecretRef, ref, f.Root)
		}
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %q", ErrSecretNotFound, ref)
	}
	if err != nil {
		return nil, fmt.Errorf("platform: read secret %q: %w", ref, err)
	}
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return nil, fmt.Errorf("%w: %q is empty", ErrSecretNotFound, ref)
	}
	return b, nil
}

// SecretRouter dispatches on the reference's scheme ("env", "file", "sm",
// ...). A scheme without a resolver is ErrSecretRef (fail closed: an sm://
// reference in a process without Secret Manager access is an error, never
// an empty secret).
type SecretRouter map[string]Secrets

// Resolve implements Secrets.
func (r SecretRouter) Resolve(ctx context.Context, ref string) ([]byte, error) {
	scheme, _, ok := strings.Cut(ref, "://")
	if !ok {
		return nil, fmt.Errorf("%w: %q has no scheme", ErrSecretRef, ref)
	}
	s, ok := r[scheme]
	if !ok || s == nil {
		return nil, fmt.Errorf("%w: no resolver for %s://", ErrSecretRef, scheme)
	}
	return s.Resolve(ctx, ref)
}

// LocalSecrets returns the resolver for env:// and file:// references,
// plus sm:// when sm is non-nil (smsecrets.Store).
func LocalSecrets(getenv func(string) string, sm Secrets) SecretRouter {
	r := SecretRouter{"env": EnvSecrets{Getenv: getenv}, "file": FileSecrets{}}
	if sm != nil {
		r["sm"] = sm
	}
	return r
}
