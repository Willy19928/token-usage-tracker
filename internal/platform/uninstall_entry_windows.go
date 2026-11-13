//go:build windows

package platform

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// uninstallKey is the per-user entry that lists the program under Settings >
// Apps > Installed apps, where its Uninstall button runs "uninstall --gui".
const uninstallKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\TokenUsage`

// RegisterUninstallEntry lists the program under Installed apps for the
// current user.
func RegisterUninstallEntry(executable, version string) error {
	executable, err := filepath.Abs(executable)
	if err != nil {
		return err
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, uninstallKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	quoted := `"` + executable + `"`
	values := map[string]string{
		"DisplayName":          "Token Usage",
		"DisplayVersion":       version,
		"DisplayIcon":          executable + ",0",
		"InstallLocation":      filepath.Dir(executable),
		"InstallDate":          time.Now().Format("20060102"),
		"UninstallString":      quoted + " uninstall --gui",
		"QuietUninstallString": quoted + " uninstall",
		"URLInfoAbout":         "https://github.com/Willy19928/token-usage-tracker",
	}
	for name, value := range values {
		if err := key.SetStringValue(name, value); err != nil {
			return err
		}
	}
	for name, value := range map[string]uint32{"NoModify": 1, "NoRepair": 1, "EstimatedSize": fileSizeKB(executable)} {
		if err := key.SetDWordValue(name, value); err != nil {
			return err
		}
	}
	return nil
}

// SyncUninstallEntry keeps the entry's version current after an in-place
// update, and adds the entry for installs made before it existed: those are
// recognised by the login start that points at this service folder.
func SyncUninstallEntry(executable, stateDir, version string) error {
	executable, err := filepath.Abs(executable)
	if err != nil {
		return err
	}
	if key, err := registry.OpenKey(registry.CURRENT_USER, uninstallKey, registry.QUERY_VALUE|registry.SET_VALUE); err == nil {
		defer key.Close()
		location, _, _ := key.GetStringValue("InstallLocation")
		if !strings.EqualFold(filepath.Clean(location), filepath.Dir(executable)) {
			return nil
		}
		if err := key.SetStringValue("DisplayVersion", version); err != nil {
			return err
		}
		return key.SetDWordValue("EstimatedSize", fileSizeKB(executable))
	} else if !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	run, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.QUERY_VALUE)
	if err != nil {
		return nil
	}
	defer run.Close()
	command, _, err := run.GetStringValue("TokenUsage")
	if err != nil || !strings.Contains(strings.ToLower(command), strings.ToLower(filepath.Join(stateDir, "token-usage-start.vbs"))) {
		return nil
	}
	return RegisterUninstallEntry(executable, version)
}

func removeUninstallEntry() error {
	err := registry.DeleteKey(registry.CURRENT_USER, uninstallKey)
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

func fileSizeKB(path string) uint32 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return uint32((info.Size() + 1023) / 1024)
}

// ConfirmDialog asks a yes/no question in a Windows message box.
func ConfirmDialog(title, text string) bool {
	const yesNo, question, defaultSecond, idYes = 0x4, 0x20, 0x100, 6
	answer, _ := windows.MessageBox(0, utf16(text), utf16(title), yesNo|question|defaultSecond)
	return answer == idYes
}

// NoticeDialog shows a message box with an OK button.
func NoticeDialog(title, text string, isError bool) {
	var icon uint32 = 0x40 // information
	if isError {
		icon = 0x10
	}
	_, _ = windows.MessageBox(0, utf16(text), utf16(title), icon)
}

func utf16(text string) *uint16 {
	pointer, err := windows.UTF16PtrFromString(strings.ReplaceAll(text, "\x00", ""))
	if err != nil {
		return nil
	}
	return pointer
}

// OwnsConsole reports whether the console window was created for this
// process alone, as when the program is opened by double-click or from
// Installed apps rather than from a terminal.
func OwnsConsole() bool {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	list := kernel32.NewProc("GetConsoleProcessList")
	var ids [4]uint32
	count, _, _ := list.Call(uintptr(unsafe.Pointer(&ids[0])), uintptr(len(ids)))
	return count == 1
}
