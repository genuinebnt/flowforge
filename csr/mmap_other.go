//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package csr

import "os"

// mapFile reads the whole file on platforms without the mmap path.
func mapFile(path string) ([]byte, func() error, error) {
	b, err := os.ReadFile(path)
	return b, nil, err
}
