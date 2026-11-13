package redisconf_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/services/internal/platform/redisconf"
)

type pki struct {
	caPEM, certFile, keyFile, caFile string
}

// newPKI writes a self-signed CA and a server certificate for 127.0.0.1
// signed by it, as Memorystore's server CA signs the instance certificate.
func newPKI(t *testing.T) pki {
	t.Helper()
	dir := t.TempDir()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test redis CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	srvKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	srvTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "redis"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, ca, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(srvKey)
	p := pki{caPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})),
		certFile: filepath.Join(dir, "server.crt"), keyFile: filepath.Join(dir, "server.key"), caFile: filepath.Join(dir, "ca.crt")}
	for f, b := range map[string][]byte{
		p.certFile: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER}),
		p.keyFile:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		p.caFile:   []byte(p.caPEM),
	} {
		if err := os.WriteFile(f, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestRegress_G_OPS_13_RedisTLS: with FF_REDIS_CA set the client speaks
// TLS verified against that CA and authenticates with FF_REDIS_AUTH; it
// refuses a server whose certificate another CA signed. It runs against a
// local TLS redis-server and is skipped when none is installed.
func TestRegress_G_OPS_13_RedisTLS(t *testing.T) {
	bin, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server not installed")
	}
	p := newPKI(t)
	port := freePort(t)
	cmd := exec.Command(bin, "--port", "0", "--tls-port", strconv.Itoa(port), "--bind", "127.0.0.1",
		"--tls-cert-file", p.certFile, "--tls-key-file", p.keyFile, "--tls-ca-cert-file", p.caFile,
		"--tls-auth-clients", "no", "--requirepass", "s3cret", "--save", "", "--appendonly", "no")
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	addr := "127.0.0.1:" + strconv.Itoa(port)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rc, err := redisconf.NewClient(redisconf.Config{Addr: addr, Auth: "s3cret", CA: p.caPEM})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	for {
		err = rc.Ping(ctx).Err()
		if err == nil || ctx.Err() != nil {
			break
		}
		if strings.Contains(out.String(), "not compiled with TLS") || strings.Contains(out.String(), "Bad directive") {
			t.Skipf("redis-server lacks TLS support: %s", out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("TLS ping: %v\n%s", err, out.String())
	}
	// The CA may also be a file path.
	rf, _ := redisconf.NewClient(redisconf.Config{Addr: addr, Auth: "s3cret", CA: p.caFile})
	defer rf.Close()
	if err := rf.Ping(ctx).Err(); err != nil {
		t.Fatalf("CA from file: %v", err)
	}
	// Wrong password, and a CA that did not sign the server: refused.
	bad, _ := redisconf.NewClient(redisconf.Config{Addr: addr, Auth: "wrong", CA: p.caPEM})
	defer bad.Close()
	if err := bad.Ping(ctx).Err(); err == nil {
		t.Fatal("wrong AUTH accepted")
	}
	other, _ := redisconf.NewClient(redisconf.Config{Addr: addr, Auth: "s3cret", CA: newPKI(t).caPEM})
	defer other.Close()
	if err := other.Ping(ctx).Err(); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("foreign CA accepted: %v", err)
	}
	// Without a CA the client speaks plain TCP, which a TLS port rejects.
	plain, _ := redisconf.NewClient(redisconf.Config{Addr: addr, Auth: "s3cret"})
	defer plain.Close()
	if err := plain.Ping(ctx).Err(); err == nil {
		t.Fatal("plain TCP to a TLS port succeeded")
	}
}

func TestRedisConfTLSConfig(t *testing.T) {
	if c, err := (redisconf.Config{Addr: "10.0.0.3:6378"}).TLSConfig(); c != nil || err != nil {
		t.Fatalf("no CA must mean no TLS: %v %v", c, err)
	}
	p := newPKI(t)
	c, err := (redisconf.Config{Addr: "10.0.0.3:6378", CA: p.caPEM}).TLSConfig()
	if err != nil || c.ServerName != "10.0.0.3" || c.MinVersion < 0x0303 || c.RootCAs == nil {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := (redisconf.Config{Addr: "x:1", CA: "-----BEGIN CERTIFICATE-----\nnope\n-----END CERTIFICATE-----"}).TLSConfig(); err == nil {
		t.Fatal("garbage CA accepted")
	}
}
