package grub

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCmdlineRemoveWord(t *testing.T) {
	if got := CmdlineRemoveWord("ro quiet splash root=/dev/sda2", "quiet"); got != "ro splash root=/dev/sda2" {
		t.Errorf("remove quiet = %q", got)
	}
	if got := CmdlineRemoveWord("ro root=/dev/sda2", "quiet"); got != "ro root=/dev/sda2" {
		t.Errorf("absent word changed cmdline = %q", got)
	}
}

func TestPatchGrubCfgContent(t *testing.T) {
	in := "menuentry x {\n\tlinux /vmlinuz root=/dev/sda2 ro quiet splash\n}\n"
	out := PatchGrubCfgContent(in)
	if strings.Contains(out, "quiet") || strings.Contains(out, "splash") {
		t.Errorf("quiet/splash not removed: %q", out)
	}
	if !strings.Contains(out, "console=tty0") || !strings.Contains(out, "console=hvc0") {
		t.Errorf("consoles not added: %q", out)
	}
	// linux16 path gets only hvc0.
	out16 := PatchGrubCfgContent("\tlinux16 /vmlinuz ro quiet\n")
	if !strings.Contains(out16, "console=hvc0") || strings.Contains(out16, "console=tty0") {
		t.Errorf("linux16 console handling wrong: %q", out16)
	}
	// No linux line -> unchanged.
	if PatchGrubCfgContent("set timeout=5\n") != "set timeout=5\n" {
		t.Error("non-linux content changed")
	}
	// Already-patched line is idempotent (no console duplication).
	once := PatchGrubCfgContent("\tlinux /vmlinuz ro\n")
	twice := PatchGrubCfgContent(once)
	if once != twice {
		t.Errorf("not idempotent: %q vs %q", once, twice)
	}
}

func TestPatchGrubDefaultsContent(t *testing.T) {
	out := PatchGrubDefaultsContent("GRUB_TIMEOUT=5\n")
	if !strings.Contains(out, `GRUB_TERMINAL_OUTPUT="console"`) {
		t.Errorf("terminal not appended: %q", out)
	}
	if !strings.Contains(out, "GRUB_CMDLINE_LINUX_DEFAULT=") {
		t.Errorf("cmdline not appended: %q", out)
	}
	// Existing keys get overridden, not duplicated.
	out2 := PatchGrubDefaultsContent("GRUB_TERMINAL_OUTPUT=\"gfxterm\"\nGRUB_CMDLINE_LINUX_DEFAULT=\"quiet\"\n")
	if strings.Count(out2, "GRUB_TERMINAL_OUTPUT=") != 1 || strings.Contains(out2, "gfxterm") {
		t.Errorf("terminal not overridden: %q", out2)
	}
	if strings.Count(out2, "GRUB_CMDLINE_LINUX_DEFAULT=") != 1 || strings.Contains(out2, "quiet") {
		t.Errorf("cmdline not overridden: %q", out2)
	}
}

// --- raw install primitives ----------------------------------------------

func TestInstallToSectors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "img")
	if err := os.WriteFile(p, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := InstallToSectors(p, []byte("BOOT"), 1, 1); err != nil {
		t.Fatalf("InstallToSectors: %v", err)
	}
	data, _ := os.ReadFile(p)
	if string(data[512:516]) != "BOOT" {
		t.Fatalf("code not written at sector 1: %q", data[512:516])
	}
	// numSectors <= 0
	if err := InstallToSectors(p, nil, 0, 0); err == nil {
		t.Error("expected error for numSectors=0")
	}
	// code too large
	if err := InstallToSectors(p, make([]byte, 600), 0, 1); err == nil {
		t.Error("expected oversize error")
	}
	// open error
	if err := InstallToSectors("/no/such/dir/img", []byte("x"), 0, 1); err == nil {
		t.Error("expected open error")
	}
}

func TestInstallMBR(t *testing.T) {
	p := filepath.Join(t.TempDir(), "img")
	if err := os.WriteFile(p, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := InstallMBR(p, []byte{0x90, 0x90}); err != nil {
		t.Fatalf("InstallMBR: %v", err)
	}
	data, _ := os.ReadFile(p)
	if data[0] != 0x90 || data[510] != 0x55 || data[511] != 0xAA {
		t.Fatalf("MBR not written: %x sig %x%x", data[0], data[510], data[511])
	}
	if err := InstallMBR(p, make([]byte, 500)); err == nil {
		t.Error("expected oversize error")
	}
	if err := InstallMBR("/no/such/dir/img", []byte{0}); err == nil {
		t.Error("expected open error")
	}
}

func TestRawReplaceAll(t *testing.T) {
	p := filepath.Join(t.TempDir(), "img")
	if err := os.WriteFile(p, []byte("aaaXXXbbbXXXccc"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	RawReplaceAll(f, []byte("XXX"), []byte("YYY"))
	// length mismatch is a no-op
	RawReplaceAll(f, []byte("ZZ"), []byte("Q"))
	f.Close()
	data, _ := os.ReadFile(p)
	if string(data) != "aaaYYYbbbYYYccc" {
		t.Fatalf("RawReplaceAll = %q", data)
	}
}
