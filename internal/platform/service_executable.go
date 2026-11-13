package platform

import (
	"regexp"
	"strings"
)

// The background service is started by a launcher written at install time
// (the Windows login script, the Linux systemd unit). It names the installed
// executable, which is how a running service finds its own installation when
// no install record exists yet.

var vbsExecutable = regexp.MustCompile(`Chr\(34\) & "((?:[^"]|"")*)" & Chr\(34\) & " daemon"`)

// launcherExecutableVBS returns the executable a token-usage-start.vbs runs.
func launcherExecutableVBS(content string) (string, bool) {
	match := vbsExecutable.FindStringSubmatch(content)
	if match == nil || match[1] == "" {
		return "", false
	}
	return strings.ReplaceAll(match[1], `""`, `"`), true
}

// launcherExecutableSystemd returns the executable of a token-usage.service
// unit whose TOKEN_USAGE_HOME is stateDir.
func launcherExecutableSystemd(content, stateDir string) (string, bool) {
	var executable, home string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "ExecStart="):
			if value, rest, ok := systemdUnquote(strings.TrimPrefix(line, "ExecStart=")); ok && strings.TrimSpace(rest) == "daemon" {
				executable = value
			}
		case strings.HasPrefix(line, "Environment="):
			if value, _, ok := systemdUnquote(strings.TrimPrefix(line, "Environment=")); ok {
				if name, path, found := strings.Cut(value, "="); found && name == "TOKEN_USAGE_HOME" {
					home = path
				}
			}
		}
	}
	if executable == "" || home != stateDir {
		return "", false
	}
	return executable, true
}

// systemdUnquote reads one double-quoted word as written by systemdQuote and
// returns it with the rest of the line.
func systemdUnquote(text string) (value, rest string, ok bool) {
	if !strings.HasPrefix(text, `"`) {
		return "", "", false
	}
	var out strings.Builder
	for index := 1; index < len(text); index++ {
		switch c := text[index]; {
		case c == '\\' && index+1 < len(text):
			index++
			out.WriteByte(text[index])
		case c == '%' && index+1 < len(text) && text[index+1] == '%':
			index++
			out.WriteByte('%')
		case c == '"':
			return out.String(), text[index+1:], true
		default:
			out.WriteByte(c)
		}
	}
	return "", "", false
}

// systemdQuote writes a word that systemd reads back as exactly path.
func systemdQuote(path string) string {
	escaped := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		`%`, `%%`,
	).Replace(path)
	return `"` + escaped + `"`
}
