//go:build unix && !linux

package metrics

import "syscall"

func statfsUnit(st *syscall.Statfs_t) int64 {
	return int64(st.Bsize)
}
