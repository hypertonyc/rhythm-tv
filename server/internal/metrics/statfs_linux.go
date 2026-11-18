//go:build linux

package metrics

import "syscall"

// Bavail и Blocks считаются в f_frsize, а не в f_bsize: у virtiofs Docker Desktop
// f_bsize = 1 МиБ при f_frsize = 4 КиБ, и место выходило завышенным в 256 раз.
func statfsUnit(st *syscall.Statfs_t) int64 {
	if st.Frsize > 0 {
		return int64(st.Frsize)
	}
	return int64(st.Bsize)
}
