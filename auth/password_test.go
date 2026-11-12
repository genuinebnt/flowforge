package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/auth"
)

func TestAuth001_Argon2(t *testing.T) {
	t.Run("policy_params", func(t *testing.T) {
		h, err := auth.NewHasher(newReader(1), auth.PolicyParams())
		if err != nil {
			t.Fatal(err)
		}
		enc, err := h.Hash("correct horse battery staple")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(enc, "$argon2id$v=19$m=19456,t=2,p=1$") {
			t.Fatalf("PHC string %q does not carry the 13.1 parameters", enc)
		}
		parts := strings.Split(enc, "$")
		if len(parts[4]) != 22 || len(parts[5]) != 43 { // 16 and 32 bytes, unpadded base64
			t.Fatalf("salt %q / hash %q have the wrong length", parts[4], parts[5])
		}
		if ok, err := h.Verify("correct horse battery staple", enc); !ok || err != nil {
			t.Fatalf("Verify(correct) = %v, %v", ok, err)
		}
		if ok, err := h.Verify("correct horse battery stapler", enc); ok || err != nil {
			t.Fatalf("Verify(wrong) = %v, %v", ok, err)
		}
		if h.NeedsRehash(enc) {
			t.Fatal("fresh hash needs rehash")
		}
	})

	t.Run("wrong_password", func(t *testing.T) {
		h := fastHasher(t, 2)
		enc, err := h.Hash("correct horse battery staple")
		if err != nil {
			t.Fatal(err)
		}
		for _, pw := range []string{"", "Correct horse battery staple", "correct horse battery staple ", "correct horse battery stapl"} {
			if ok, err := h.Verify(pw, enc); ok || err != nil {
				t.Errorf("Verify(%q) = %v, %v; want false, nil", pw, ok, err)
			}
		}
	})

	t.Run("rehash_on_parameter_change", func(t *testing.T) {
		old := fastHasher(t, 3)
		enc, err := old.Hash("correct horse battery staple")
		if err != nil {
			t.Fatal(err)
		}
		p := fastParams()
		p.Time = 2
		cur, err := auth.NewHasher(newReader(4), p, auth.AllowWeakParams())
		if err != nil {
			t.Fatal(err)
		}
		if !cur.NeedsRehash(enc) {
			t.Fatal("hash made with old parameters does not need rehash")
		}
		if ok, _ := cur.Verify("correct horse battery staple", enc); !ok {
			t.Fatal("old hash no longer verifies with its own parameters")
		}
		fresh, err := cur.Hash("correct horse battery staple")
		if err != nil {
			t.Fatal(err)
		}
		if cur.NeedsRehash(fresh) || !strings.Contains(fresh, "t=2") {
			t.Fatalf("rehashed %q still needs rehash", fresh)
		}
		if !cur.NeedsRehash("garbage") {
			t.Fatal("unparsable hash does not need rehash")
		}
	})

	t.Run("weak_params_refused", func(t *testing.T) {
		weak := auth.PolicyParams()
		weak.Memory = 16 * 1024
		if _, err := auth.NewHasher(newReader(5), weak); !errors.Is(err, auth.ErrWeakParams) {
			t.Fatalf("NewHasher(16 MiB) error = %v, want ErrWeakParams", err)
		}
		if _, err := auth.NewHasher(newReader(5), weak, auth.AllowWeakParams()); err != nil {
			t.Fatalf("AllowWeakParams: %v", err)
		}
		if !auth.PolicyParams().MeetsPolicy() || weak.MeetsPolicy() {
			t.Fatal("MeetsPolicy disagrees with the 13.1 floor")
		}
	})

	t.Run("fresh_salt_per_hash", func(t *testing.T) {
		h := fastHasher(t, 6)
		a, _ := h.Hash("correct horse battery staple")
		b, _ := h.Hash("correct horse battery staple")
		if a == b {
			t.Fatal("two hashes of one password are identical")
		}
	})
}

func TestPHCRejectsMalformed(t *testing.T) {
	h := fastHasher(t, 7)
	good, err := h.Hash("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(good, "$")
	salt, key := parts[4], parts[5]
	bad := []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=64,t=1,p=1$" + salt + "$" + key,
		"$argon2id$v=16$m=64,t=1,p=1$" + salt + "$" + key,
		"$argon2id$v=19$t=1,m=64,p=1$" + salt + "$" + key,
		"$argon2id$v=19$m=064,t=1,p=1$" + salt + "$" + key,
		"$argon2id$v=19$m=64,t=1$" + salt + "$" + key,
		"$argon2id$v=19$m=64,t=1,p=0$" + salt + "$" + key,
		"$argon2id$v=19$m=99999999,t=1,p=1$" + salt + "$" + key,
		"$argon2id$v=19$m=64,t=1000,p=1$" + salt + "$" + key,
		"$argon2id$v=19$m=64,t=1,p=1$" + salt + "==$" + key,
		"$argon2id$v=19$m=64,t=1,p=1$AAAA$" + key,
		"$argon2id$v=19$m=64,t=1,p=1$" + salt + "$" + key + "$extra",
		"$argon2id$v=19$m=64,t=1,p=1$" + salt + "$" + key[:10],
	}
	for _, enc := range bad {
		if ok, err := h.Verify("correct horse battery staple", enc); ok || !errors.Is(err, auth.ErrInvalidHash) {
			t.Errorf("Verify(%q) = %v, %v; want false, ErrInvalidHash", enc, ok, err)
		}
	}
}

type failingChecker struct{}

func (failingChecker) IsBreached(context.Context, string) (bool, error) {
	return false, errors.New("breach service down")
}

func TestPasswordPolicy(t *testing.T) {
	p := auth.PasswordPolicy{Breached: auth.NewBreachedSet("password1234", "correct horse battery staple")}
	tests := []struct {
		name string
		pw   string
		want error
	}{
		{"ok", "a long enough passphrase", nil},
		{"exactly 12", "abcdefghijkl", nil},
		{"11 chars", "abcdefghijk", auth.ErrPasswordTooShort},
		{"12 multibyte runes", strings.Repeat("é", 12), nil},
		{"128 chars", strings.Repeat("a", 128), nil},
		{"129 chars", strings.Repeat("a", 129), auth.ErrPasswordTooLong},
		{"no composition rules", "aaaaaaaaaaaa", nil},
		{"invalid utf-8", "abcdefghijk\xff", auth.ErrPasswordEncoding},
		{"breached", "correct horse battery staple", auth.ErrPasswordBreached},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := p.Check(context.Background(), tt.pw)
			if tt.want == nil && err != nil || tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("Check(%q) = %v, want %v", tt.pw, err, tt.want)
			}
			if tt.want != nil && !errors.Is(err, auth.ErrPasswordPolicy) {
				t.Fatalf("Check(%q) = %v does not match ErrPasswordPolicy", tt.pw, err)
			}
		})
	}
	// Fail closed when the breached list cannot be consulted.
	for _, c := range []auth.BreachedChecker{nil, failingChecker{}} {
		if err := (auth.PasswordPolicy{Breached: c}).Check(context.Background(), "a long enough passphrase"); err == nil {
			t.Errorf("Check with checker %T accepted the password", c)
		}
	}
}

// BenchmarkAuth01_Hash is b_auth_01 hashing: Argon2id with the policy
// parameters (target 200-500 ms on production hardware is a tuning goal
// for the parameters, not a property of the code).
func BenchmarkAuth01_Hash(b *testing.B) {
	h, err := auth.NewHasher(newReader(1), auth.PolicyParams())
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := h.Hash("correct horse battery staple"); err != nil {
			b.Fatal(err)
		}
	}
}
