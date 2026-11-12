package csr

import "fmt"

// Open loads and fully validates a snapshot file. On Linux and the BSDs
// the file is memory mapped read-only and the arrays are used in place;
// elsewhere it is read into memory. Call Close to release the mapping once
// no reader uses the Graph any more.
//
// Snapshot files are immutable once written (they are named by
// watermark); truncating or rewriting a file while it is mapped is
// undefined behaviour at the operating-system level (SIGBUS).
func Open(path string) (*Graph, error) { return OpenWith(path, OpenOptions{}) }

// OpenWith is Open with options.
func OpenWith(path string, opts OpenOptions) (*Graph, error) {
	data, release, err := mapFile(path)
	if err != nil {
		return nil, fmt.Errorf("csr: open %s: %w", path, err)
	}
	g, err := decode(data, opts)
	if err != nil {
		if release != nil {
			_ = release()
		}
		return nil, fmt.Errorf("csr: open %s: %w", path, err)
	}
	g.release = release
	return g, nil
}
