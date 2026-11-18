//go:build linux

package reclaim

import (
	"syscall"
	"testing"
)

func TestStatfsUnitIsFragmentSize(t *testing.T) {
	cases := []struct {
		name          string
		bsize, frsize int64
		want          int64
	}{
		{"virtiofs Docker Desktop", 1 << 20, 4096, 4096},
		{"ext4", 4096, 4096, 4096},
		{"frsize не заполнен", 4096, 0, 4096},
	}
	for _, c := range cases {
		st := syscall.Statfs_t{Bsize: c.bsize, Frsize: c.frsize}
		if got := statfsUnit(&st); got != c.want {
			t.Errorf("%s: statfsUnit = %d, want %d", c.name, got, c.want)
		}
	}
}
