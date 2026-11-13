// Package redisconf builds go-redis client options from the spec 17.5
// variables FF_REDIS_ADDR, FF_REDIS_AUTH and FF_REDIS_CA, so every Redis
// client (gateway revocations, lockouts and limits; engine limiter) speaks
// TLS to Memorystore the same way (G-OPS-13). It is a sub-package so
// services without Redis do not link go-redis.
package redisconf

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/redis/go-redis/v9"
)

// Config is the Redis connection configuration.
type Config struct {
	Addr string // FF_REDIS_ADDR, host:port
	Auth string // FF_REDIS_AUTH, the AUTH string (empty: no AUTH)
	// CA is FF_REDIS_CA: PEM text of the CA that signed the server
	// certificate (Terraform mounts Memorystore's server_ca_pem from the
	// ff-redis-ca secret as the variable's value), or the path of a PEM
	// file. Non-empty enables TLS verified against exactly this CA.
	CA string
	// ServerName overrides the name verified in the server certificate
	// (default: the host of Addr; an IP address is matched against the
	// certificate's IP SANs, which Memorystore server certificates carry).
	ServerName string
}

// TLSConfig returns the client TLS configuration for c, or nil when CA is
// empty (plain TCP, local Redis).
func (c Config) TLSConfig() (*tls.Config, error) {
	if c.CA == "" {
		return nil, nil
	}
	pem := []byte(c.CA)
	if !strings.Contains(c.CA, "-----BEGIN") {
		b, err := os.ReadFile(c.CA)
		if err != nil {
			return nil, fmt.Errorf("redisconf: FF_REDIS_CA is neither PEM nor a readable file: %w", err)
		}
		pem = b
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("redisconf: FF_REDIS_CA holds no PEM certificate")
	}
	name := c.ServerName
	if name == "" {
		host, _, err := net.SplitHostPort(c.Addr)
		if err != nil {
			return nil, fmt.Errorf("redisconf: FF_REDIS_ADDR %q: %w", c.Addr, err)
		}
		name = host
	}
	// Only this CA is trusted (not the system roots): the Memorystore CA is
	// private to the instance, and trusting public roots as well would let
	// any publicly issued certificate for the address pass.
	return &tls.Config{RootCAs: pool, ServerName: name, MinVersion: tls.VersionTLS12}, nil
}

// Options returns go-redis options for c.
func (c Config) Options() (*redis.Options, error) {
	if c.Addr == "" {
		return nil, errors.New("redisconf: no address")
	}
	t, err := c.TLSConfig()
	if err != nil {
		return nil, err
	}
	return &redis.Options{Addr: c.Addr, Password: c.Auth, TLSConfig: t}, nil
}

// NewClient returns a client for c.
func NewClient(c Config) (*redis.Client, error) {
	o, err := c.Options()
	if err != nil {
		return nil, err
	}
	return redis.NewClient(o), nil
}
