//go:build !linux

package hw

import "errors"

// statfs is only implemented on Linux; the cluster only supports Linux
// workers, this stub just keeps development builds compiling elsewhere.
func statfs(path string) (total, free uint64, err error) {
	return 0, 0, errors.New("statfs not supported on this OS")
}
