//go:build unix

package diskusage

import "syscall"

func of(path string) (Usage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Usage{}, err
	}
	bs := uint64(st.Bsize) //nolint:unconvert // int64 on linux, uint32 on darwin
	return Usage{Total: st.Blocks * bs, Free: st.Bavail * bs}, nil
}
