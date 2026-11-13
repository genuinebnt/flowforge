// Package smsecrets is the Google Secret Manager adapter of
// platform.Secrets (spec 17.1): it resolves sm:// references and, for the
// gateway's signing-key store, creates secrets and versions. It is a
// separate package so services that never touch Secret Manager do not link
// the Cloud SDK (docs/CONVENTIONS.md, adapters in sub-packages).
package smsecrets

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"regexp"
	"strings"
	"sync"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/genuinebnt/flowforge/services/internal/platform"
)

// API is the subset of the Secret Manager client the adapter uses;
// *secretmanager.Client implements it and tests substitute a fake.
type API interface {
	AccessSecretVersion(ctx context.Context, req *secretmanagerpb.AccessSecretVersionRequest, opts ...gax.CallOption) (*secretmanagerpb.AccessSecretVersionResponse, error)
	CreateSecret(ctx context.Context, req *secretmanagerpb.CreateSecretRequest, opts ...gax.CallOption) (*secretmanagerpb.Secret, error)
	AddSecretVersion(ctx context.Context, req *secretmanagerpb.AddSecretVersionRequest, opts ...gax.CallOption) (*secretmanagerpb.SecretVersion, error)
}

// refRE is the full form sm://projects/P/secrets/S/versions/V (V a number
// or "latest"). Secret ids follow Secret Manager's [A-Za-z0-9_-]{1,255}.
var (
	refRE      = regexp.MustCompile(`^sm://(projects/[a-z0-9-]{1,63}/secrets/[A-Za-z0-9_-]{1,255}/versions/(?:latest|[0-9]+))$`)
	secretIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
)

// Store resolves sm:// references and writes secrets. The client is
// created on first use (Application Default Credentials), so a process
// that holds sm:// references but never resolves one needs no credentials,
// and a failed start is retried on the next call. Safe for concurrent use.
type Store struct {
	// Project is the default project for short references (sm://NAME =
	// sm://projects/<Project>/secrets/NAME/versions/latest) and for Put.
	Project string
	// API overrides the client (tests).
	API API

	mu  sync.Mutex
	cli *secretmanager.Client
}

var _ platform.Secrets = (*Store)(nil)

func (s *Store) api(ctx context.Context) (API, error) {
	if s.API != nil {
		return s.API, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cli == nil {
		c, err := secretmanager.NewClient(ctx)
		if err != nil {
			return nil, fmt.Errorf("smsecrets: client: %w", err)
		}
		s.cli = c
	}
	return s.cli, nil
}

// Close releases the client.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cli == nil {
		return nil
	}
	err := s.cli.Close()
	s.cli = nil
	return err
}

// Name returns the version resource name of ref, expanding the short
// form with Project.
func (s *Store) Name(ref string) (string, error) {
	if m := refRE.FindStringSubmatch(ref); m != nil {
		return m[1], nil
	}
	short, ok := strings.CutPrefix(ref, "sm://")
	if ok && s.Project != "" && secretIDRE.MatchString(short) {
		return "projects/" + s.Project + "/secrets/" + short + "/versions/latest", nil
	}
	return "", fmt.Errorf("%w: %q (want sm://projects/P/secrets/S/versions/V)", platform.ErrSecretRef, ref)
}

// Resolve implements platform.Secrets. A missing secret or version, or a
// disabled or destroyed one, is platform.ErrSecretNotFound; a payload
// whose CRC32C does not match is an error (corruption in transit).
func (s *Store) Resolve(ctx context.Context, ref string) ([]byte, error) {
	name, err := s.Name(ref)
	if err != nil {
		return nil, err
	}
	api, err := s.api(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := api.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: name})
	switch status.Code(err) {
	case codes.OK:
	case codes.NotFound, codes.FailedPrecondition:
		return nil, fmt.Errorf("%w: %q: %v", platform.ErrSecretNotFound, ref, err)
	default:
		return nil, fmt.Errorf("smsecrets: access %s: %w", name, err)
	}
	data := resp.GetPayload().GetData()
	if c := resp.GetPayload().DataCrc32C; c != nil && int64(crc32.Checksum(data, castagnoli)) != *c {
		return nil, fmt.Errorf("smsecrets: %s: payload checksum mismatch", name)
	}
	return data, nil
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Put stores value as a new version of secret id in Project, creating the
// secret (automatic replication) if needed, and returns the full sm://
// reference of the version written. It needs secretmanager.secrets.create
// and secretVersions.add on the project.
func (s *Store) Put(ctx context.Context, id string, value []byte) (string, error) {
	if s.Project == "" {
		return "", errors.New("smsecrets: Put needs a project")
	}
	if !secretIDRE.MatchString(id) {
		return "", fmt.Errorf("smsecrets: invalid secret id %q", id)
	}
	api, err := s.api(ctx)
	if err != nil {
		return "", err
	}
	parent := "projects/" + s.Project
	_, err = api.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: parent, SecretId: id, Secret: &secretmanagerpb.Secret{
		Replication: &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}},
		Labels:      map[string]string{"app": "flowforge"},
	}})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return "", fmt.Errorf("smsecrets: create %s: %w", id, err)
	}
	crc := int64(crc32.Checksum(value, castagnoli))
	v, err := api.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: parent + "/secrets/" + id,
		Payload: &secretmanagerpb.SecretPayload{Data: value, DataCrc32C: &crc}})
	if err != nil {
		return "", fmt.Errorf("smsecrets: add version to %s: %w", id, err)
	}
	return "sm://" + v.GetName(), nil
}
