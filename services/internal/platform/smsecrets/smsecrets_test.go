package smsecrets_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/platform/smsecrets"
)

// fakeSM is an in-memory Secret Manager: secrets hold numbered versions.
type fakeSM struct {
	mu      sync.Mutex
	secrets map[string][][]byte // projects/P/secrets/S -> versions
}

func newFake() *fakeSM { return &fakeSM{secrets: map[string][][]byte{}} }

func (f *fakeSM) AccessSecretVersion(_ context.Context, r *secretmanagerpb.AccessSecretVersionRequest, _ ...gax.CallOption) (*secretmanagerpb.AccessSecretVersionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	secret, ver, _ := strings.Cut(r.GetName(), "/versions/")
	vs := f.secrets[secret]
	n := len(vs)
	if ver != "latest" {
		_, _ = fmt.Sscan(ver, &n)
	}
	if n < 1 || n > len(vs) {
		return nil, status.Error(codes.NotFound, "no such version")
	}
	return &secretmanagerpb.AccessSecretVersionResponse{Name: r.GetName(), Payload: &secretmanagerpb.SecretPayload{Data: vs[n-1]}}, nil
}

func (f *fakeSM) CreateSecret(_ context.Context, r *secretmanagerpb.CreateSecretRequest, _ ...gax.CallOption) (*secretmanagerpb.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := r.GetParent() + "/secrets/" + r.GetSecretId()
	if _, ok := f.secrets[name]; ok {
		return nil, status.Error(codes.AlreadyExists, "exists")
	}
	f.secrets[name] = nil
	return &secretmanagerpb.Secret{Name: name}, nil
}

func (f *fakeSM) AddSecretVersion(_ context.Context, r *secretmanagerpb.AddSecretVersionRequest, _ ...gax.CallOption) (*secretmanagerpb.SecretVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.secrets[r.GetParent()]; !ok {
		return nil, status.Error(codes.NotFound, "no secret")
	}
	f.secrets[r.GetParent()] = append(f.secrets[r.GetParent()], r.GetPayload().GetData())
	return &secretmanagerpb.SecretVersion{Name: fmt.Sprintf("%s/versions/%d", r.GetParent(), len(f.secrets[r.GetParent()]))}, nil
}

// TestRegress_G_SVC_04_SecretManagerRefs: sm:// references resolve through
// Secret Manager (full and short forms, pinned and latest versions), and
// unknown or malformed ones fail closed.
func TestRegress_G_SVC_04_SecretManagerRefs(t *testing.T) {
	ctx := context.Background()
	s := &smsecrets.Store{Project: "ff-lab", API: newFake()}
	ref1, err := s.Put(ctx, "ff-webhook-ops", []byte("first"))
	if err != nil || ref1 != "sm://projects/ff-lab/secrets/ff-webhook-ops/versions/1" {
		t.Fatalf("put: %q %v", ref1, err)
	}
	if _, err := s.Put(ctx, "ff-webhook-ops", []byte("second")); err != nil {
		t.Fatal(err)
	}
	for ref, want := range map[string]string{
		ref1: "first",
		"sm://projects/ff-lab/secrets/ff-webhook-ops/versions/latest": "second",
		"sm://ff-webhook-ops": "second",
	} {
		if got, err := s.Resolve(ctx, ref); err != nil || string(got) != want {
			t.Errorf("%s = %q %v, want %q", ref, got, err, want)
		}
	}
	if _, err := s.Resolve(ctx, "sm://projects/ff-lab/secrets/missing/versions/latest"); !errors.Is(err, platform.ErrSecretNotFound) {
		t.Fatalf("missing: %v", err)
	}
	for _, bad := range []string{"sm://", "sm://projects/x/secrets/a/versions/v1", "sm://../etc", "env://X"} {
		if _, err := s.Resolve(ctx, bad); !errors.Is(err, platform.ErrSecretRef) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	// Through the router a notifier uses: sm:// with the adapter, env://
	// and file:// locally.
	r := platform.LocalSecrets(func(k string) string { return map[string]string{"HOOK": "from-env"}[k] }, s)
	if got, err := r.Resolve(ctx, "env://HOOK"); err != nil || string(got) != "from-env" {
		t.Fatalf("env: %q %v", got, err)
	}
	if got, err := r.Resolve(ctx, ref1); err != nil || string(got) != "first" {
		t.Fatalf("sm via router: %q %v", got, err)
	}
	if _, err := platform.LocalSecrets(nil, nil).Resolve(ctx, ref1); !errors.Is(err, platform.ErrSecretRef) {
		t.Fatalf("sm without an adapter must fail closed: %v", err)
	}
}
