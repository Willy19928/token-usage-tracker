package platform

import "testing"

func TestLauncherExecutableVBS(t *testing.T) {
	content := `Set shell = CreateObject("WScript.Shell")` + "\r\n" +
		`shell.Environment("Process")("TOKEN_USAGE_HOME") = "C:\Users\A\AppData\Local\token-usage"` + "\r\n" +
		`shell.Run Chr(34) & "C:\Users\A ""B""\AppData\Local\Programs\token-usage\token-usage.exe" & Chr(34) & " daemon", 0, False` + "\r\n"
	got, ok := launcherExecutableVBS(content)
	if want := `C:\Users\A "B"\AppData\Local\Programs\token-usage\token-usage.exe`; !ok || got != want {
		t.Fatalf("got %q %v, want %q", got, ok, want)
	}
	if _, ok := launcherExecutableVBS(`shell.Run "other.exe"`); ok {
		t.Fatal("parsed a launcher that does not start the service")
	}
}

func TestLauncherExecutableSystemd(t *testing.T) {
	state := "/home/a/.local/share/token-usage"
	unit := "[Service]\nType=simple\nEnvironment=" + systemdQuote("TOKEN_USAGE_HOME="+state) +
		"\nExecStart=" + systemdQuote("/home/a/.local/bin/token-usage") + " daemon\n"
	got, ok := launcherExecutableSystemd(unit, state)
	if !ok || got != "/home/a/.local/bin/token-usage" {
		t.Fatalf("got %q %v", got, ok)
	}
	weird := `/home/a/my "bin" 100%\x/token-usage`
	unit = "Environment=" + systemdQuote("TOKEN_USAGE_HOME="+state) + "\nExecStart=" + systemdQuote(weird) + " daemon\n"
	if got, ok := launcherExecutableSystemd(unit, state); !ok || got != weird {
		t.Fatalf("escaped path: got %q %v", got, ok)
	}
	if _, ok := launcherExecutableSystemd(unit, "/other/state"); ok {
		t.Fatal("accepted a unit for another service folder")
	}
}
