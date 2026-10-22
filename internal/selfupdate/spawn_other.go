//go:build !windows

package selfupdate

// SpawnMSI 在非 Windows 平台不可用：本产品的原生形态只面向 Windows（Docker 形态更新 = 拉新镜像）。
func SpawnMSI(msiPath, logPath string) (Waiter, error) { return nil, ErrNotRunnable }
