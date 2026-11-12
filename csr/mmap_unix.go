//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package csr

import (
	"fmt"
	"math"
	"os"
	"syscall"
)

// mapFile maps path read-only. An empty file yields an empty slice and no
// mapping (mmap rejects zero lengths); decode then reports it as too short.
func mapFile(path string) ([]byte, func() error, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	size := st.Size()
	if size == 0 {
		return []byte{}, nil, nil
	}
	if size < 0 || size > math.MaxInt {
		return nil, nil, fmt.Errorf("file size %d not mappable", size)
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, nil, fmt.Errorf("mmap: %w", err)
	}
	return data, func() error { return syscall.Munmap(data) }, nil
}
