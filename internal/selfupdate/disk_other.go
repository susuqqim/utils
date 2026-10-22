//go:build !windows

package selfupdate

import "syscall"

// diskFree 用 statfs 取可用字节数（Linux/macOS 上跑单测与门禁用得到）。
func diskFree(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
