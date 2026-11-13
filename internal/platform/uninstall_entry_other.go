//go:build !windows

package platform

// The Installed apps entry and message boxes exist only on Windows.

func RegisterUninstallEntry(executable, version string) error       { return nil }
func SyncUninstallEntry(executable, stateDir, version string) error { return nil }
func ConfirmDialog(title, text string) bool                         { return false }
func NoticeDialog(title, text string, isError bool)                 {}
func OwnsConsole() bool                                             { return false }
