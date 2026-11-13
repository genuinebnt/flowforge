// Package keystore holds the durable identity.KeyStore adapters for the
// gateway's signing keys (spec 13.7; G-SVC-03, G-OPS-12). The in-memory
// identity.MemKeyStore loses every key on restart, so instances cannot
// verify each other's tokens; these keep the private half outside the
// process:
//
//   - Provisioned: an operator-provisioned key read through platform
//     secret references. FromEnv is the Terraform layout
//     (FF_SIGNING_KID plus FF_SIGNING_KEY_<KID>), FromSecretManager reads
//     ff-signing-key-<kid> directly. The KeyManager signs with it and does
//     not generate keys; rotation is operator-driven.
//   - Dir: a directory of key files, written by the KeyManager on
//     rotation; several local processes sharing one directory and one
//     database share keys.
//   - SecretManager: secrets created by the KeyManager on rotation
//     (needs secretmanager.secrets.create on the project).
//
// identity.signing_keys stores only the returned reference
// (private_key_ref, spec 10.2).
package keystore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/genuinebnt/flowforge/services/gateway/identity"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/platform/smsecrets"
)

// ErrReadOnly is returned by PutPrivateKey of a store whose keys an
// operator provisions.
var ErrReadOnly = errors.New("keystore: keys are provisioned by the operator; rotate by provisioning a new kid")

// kidRE bounds key ids: they become file names and secret ids.
var kidRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// resolveSeed reads ref through s and decodes the seed; a missing secret
// is identity.ErrNotFound, which the KeyManager treats as "private half
// gone" (it stops signing with that key).
func resolveSeed(ctx context.Context, s platform.Secrets, ref string) ([]byte, error) {
	b, err := s.Resolve(ctx, ref)
	if errors.Is(err, platform.ErrSecretNotFound) {
		return nil, fmt.Errorf("%w: %v", identity.ErrNotFound, err)
	}
	if err != nil {
		return nil, err
	}
	seed, err := platform.DecodeSeed(string(b))
	if err != nil {
		return nil, fmt.Errorf("keystore: %s: %w", ref, err)
	}
	return seed, nil
}

// Provisioned is an operator-provisioned signing key: KID is the key the
// gateway signs with and Ref the secret reference of its seed. Other
// references (keys provisioned earlier, still retiring) resolve through
// Secrets too. It implements identity.ProvisionedKeys.
type Provisioned struct {
	KID     string
	Ref     string
	Secrets platform.Secrets
}

var _ identity.ProvisionedKeys = Provisioned{}

// FromEnv is the Terraform layout: FF_SIGNING_KID names the key, its
// seed is in FF_SIGNING_KEY_<KID> (platform.SigningKeyVar), or for
// compatibility in FF_SIGNING_KEY. Previous keys stay readable while
// their FF_SIGNING_KEY_<KID> variables remain mounted.
func FromEnv(kid string, getenv func(string) string) (Provisioned, error) {
	if !kidRE.MatchString(kid) {
		return Provisioned{}, fmt.Errorf("keystore: FF_SIGNING_KID %q is not a valid key id", kid)
	}
	name, v := platform.SigningKeyEnv(kid, getenv)
	if v == "" {
		return Provisioned{}, fmt.Errorf("keystore: neither %s nor FF_SIGNING_KEY is set", platform.SigningKeyVar(kid))
	}
	return Provisioned{KID: kid, Ref: "env://" + name, Secrets: platform.EnvSecrets{Getenv: getenv}}, nil
}

// SigningSecretID is the Secret Manager secret of a kid (Terraform:
// ff-signing-key-<kid>).
func SigningSecretID(kid string) string { return "ff-signing-key-" + kid }

// FromSecretManager reads the current key from
// sm://projects/<project>/secrets/ff-signing-key-<kid>/versions/latest
// (secretAccessor is enough).
func FromSecretManager(kid, project string, sm *smsecrets.Store) (Provisioned, error) {
	if !kidRE.MatchString(kid) || project == "" {
		return Provisioned{}, fmt.Errorf("keystore: Secret Manager keys need a valid FF_SIGNING_KID and a project")
	}
	return Provisioned{KID: kid, Ref: "sm://projects/" + project + "/secrets/" + SigningSecretID(kid) + "/versions/latest",
		Secrets: platform.SecretRouter{"sm": sm}}, nil
}

// CurrentKey implements identity.ProvisionedKeys.
func (p Provisioned) CurrentKey(ctx context.Context) (identity.ProvisionedKey, bool, error) {
	seed, err := resolveSeed(ctx, p.Secrets, p.Ref)
	if err != nil {
		return identity.ProvisionedKey{}, false, fmt.Errorf("keystore: current key %s: %w", p.KID, err)
	}
	return identity.ProvisionedKey{KID: p.KID, Ref: p.Ref, Seed: seed}, true, nil
}

// PutPrivateKey implements identity.KeyStore: provisioned stores are read
// only.
func (p Provisioned) PutPrivateKey(context.Context, string, []byte) (string, error) {
	return "", ErrReadOnly
}

// GetPrivateKey implements identity.KeyStore.
func (p Provisioned) GetPrivateKey(ctx context.Context, ref string) ([]byte, error) {
	return resolveSeed(ctx, p.Secrets, ref)
}

// Dir keeps one file per key, <Path>/<kid>.key holding the hex seed with
// mode 0600, and references it as file:///abs/<kid>.key. Reads are
// confined to Path, so a tampered private_key_ref cannot read other files.
type Dir struct{ Path string }

var _ identity.KeyStore = Dir{}

func (d Dir) abs() (string, error) {
	if d.Path == "" {
		return "", errors.New("keystore: empty key directory")
	}
	return filepath.Abs(d.Path)
}

// PutPrivateKey implements identity.KeyStore. The file is written to a
// temporary name and renamed, so a concurrent reader never sees a partial
// key.
func (d Dir) PutPrivateKey(_ context.Context, kid string, seed []byte) (string, error) {
	if !kidRE.MatchString(kid) {
		return "", fmt.Errorf("keystore: invalid kid %q", kid)
	}
	dir, err := d.abs()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "."+kid+".*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err := fmt.Fprintf(f, "%x\n", seed); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	path := filepath.Join(dir, kid+".key")
	if err := os.Rename(f.Name(), path); err != nil {
		return "", err
	}
	return "file://" + path, nil
}

// GetPrivateKey implements identity.KeyStore.
func (d Dir) GetPrivateKey(ctx context.Context, ref string) ([]byte, error) {
	dir, err := d.abs()
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(ref, "file://") {
		return nil, fmt.Errorf("%w: %s is not a key file", identity.ErrNotFound, ref)
	}
	return resolveSeed(ctx, platform.FileSecrets{Root: dir}, ref)
}

// SecretManager stores each generated key as secret ff-signing-key-<kid>
// in Project, so KeyManager.Rotate works across instances and restarts.
type SecretManager struct{ SM *smsecrets.Store }

var _ identity.KeyStore = SecretManager{}

// PutPrivateKey implements identity.KeyStore; the reference names the
// exact version written, so a later version of the same secret cannot
// change what a stored key signs with.
func (s SecretManager) PutPrivateKey(ctx context.Context, kid string, seed []byte) (string, error) {
	if !kidRE.MatchString(kid) {
		return "", fmt.Errorf("keystore: invalid kid %q", kid)
	}
	return s.SM.Put(ctx, SigningSecretID(kid), fmt.Appendf(nil, "%x", seed))
}

// GetPrivateKey implements identity.KeyStore.
func (s SecretManager) GetPrivateKey(ctx context.Context, ref string) ([]byte, error) {
	return resolveSeed(ctx, s.SM, ref)
}
