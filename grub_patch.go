package grub

import "strings"

// CmdlineRemoveWord removes a single word from a kernel command line.
// It preserves the order of other tokens and returns the original string
// unchanged if the word is not present.
func CmdlineRemoveWord(cmdline, word string) string {
	if !strings.Contains(cmdline, word) {
		return cmdline
	}
	parts := strings.Fields(cmdline)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == word {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, " ")
}

// PatchGrubCfgContent edits a single grub.cfg "linux" or "linux16" line to
// remove quiet/splash tokens and ensure console arguments are present. If no
// linux line is found the input is returned unchanged. Indentation is
// preserved and existing console arguments are not duplicated.
func PatchGrubCfgContent(input string) string {
	lines := strings.Split(input, "\n")
	modified := false
	for i, l := range lines {
		trimmed := strings.TrimLeft(l, " \t")
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "linux ") || strings.HasPrefix(trimmed, "linux16 ") {
			indent := l[:len(l)-len(trimmed)]
			fields := strings.Fields(trimmed)
			prefix := fields[0]
			cmdline := ""
			if len(fields) > 1 {
				cmdline = strings.Join(fields[1:], " ")
			}
			// Remove quiet and splash
			cmdline = CmdlineRemoveWord(cmdline, "quiet")
			cmdline = CmdlineRemoveWord(cmdline, "splash")
			// Add appropriate consoles without duplicating
			if prefix == "linux16" {
				if !strings.Contains(cmdline, "console=hvc0") {
					cmdline = strings.TrimSpace(cmdline + " console=hvc0")
				}
			} else {
				if !strings.Contains(cmdline, "console=tty0") {
					cmdline = strings.TrimSpace(cmdline + " console=tty0")
				}
				if !strings.Contains(cmdline, "console=hvc0") {
					cmdline = strings.TrimSpace(cmdline + " console=hvc0")
				}
			}
			cmdline = strings.TrimSpace(cmdline)
			lines[i] = indent + prefix
			if cmdline != "" {
				lines[i] += " " + cmdline
			}
			modified = true
		}
	}
	if !modified {
		return input
	}
	return strings.Join(lines, "\n")
}

// PatchGrubDefaultsContent ensures `/etc/default/grub` contains the desired
// terminal and kernel cmdline settings. Keys are overridden if present or
// appended if missing.
func PatchGrubDefaultsContent(input string) string {
	wantTerminal := `GRUB_TERMINAL_OUTPUT="console"`
	wantCmdline := `GRUB_CMDLINE_LINUX_DEFAULT="console=tty0 console=hvc0"`

	lines := strings.Split(input, "\n")
	foundTerminal := false
	foundCmdline := false
	for i, l := range lines {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "GRUB_TERMINAL_OUTPUT=") {
			lines[i] = wantTerminal
			foundTerminal = true
			continue
		}
		if strings.HasPrefix(trimmed, "GRUB_CMDLINE_LINUX_DEFAULT=") {
			lines[i] = wantCmdline
			foundCmdline = true
			continue
		}
	}
	if !foundTerminal {
		lines = append(lines, wantTerminal)
	}
	if !foundCmdline {
		lines = append(lines, wantCmdline)
	}
	return strings.Join(lines, "\n")
}
