// Package gcpauth supplies Google-signed ID tokens for calls to Cloud Run
// services (spec 17.3: non-public services require an ID token whose
// audience is the callee's URL). It is the production platform.IDTokenSource;
// a sub-package so package platform and its tests do not link the Google
// auth libraries, and tests inject a fake source instead.
package gcpauth

import (
	"context"
	"fmt"
	"sync"

	"golang.org/x/oauth2"
	"google.golang.org/api/idtoken"

	"github.com/genuinebnt/flowforge/services/internal/platform"
)

// IDTokens mints ID tokens through Application Default Credentials (on
// Cloud Run, the metadata server and the service's runtime service
// account). One oauth2.TokenSource is kept per audience; it caches the
// token until shortly before expiry, so a call costs a metadata request
// about once an hour per callee. Safe for concurrent use.
type IDTokens struct {
	mu      sync.Mutex
	sources map[string]oauth2.TokenSource
}

var _ platform.IDTokenSource = (*IDTokens)(nil)

// New returns an empty token source set.
func New() *IDTokens { return &IDTokens{sources: map[string]oauth2.TokenSource{}} }

// IDToken implements platform.IDTokenSource.
func (t *IDTokens) IDToken(ctx context.Context, audience string) (string, error) {
	t.mu.Lock()
	src, ok := t.sources[audience]
	if !ok {
		// The source outlives this call, so it must not inherit the call's
		// deadline or cancellation.
		s, err := idtoken.NewTokenSource(context.WithoutCancel(ctx), audience)
		if err != nil {
			t.mu.Unlock()
			return "", fmt.Errorf("gcpauth: ID token source for %s: %w", audience, err)
		}
		src = oauth2.ReuseTokenSource(nil, s)
		t.sources[audience] = src
	}
	t.mu.Unlock()
	tok, err := src.Token()
	if err != nil {
		return "", fmt.Errorf("gcpauth: ID token for %s: %w", audience, err)
	}
	return tok.AccessToken, nil
}
