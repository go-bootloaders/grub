package grub

import (
	"strings"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

const sampleGrubCfg = `set default=0
set timeout=5
menuentry 'Debian' {
	linux /boot/vmlinuz-6.1.0 root=/dev/sda2 ro quiet splash
	initrd /boot/initrd.img-6.1.0
}
`

// TestPatchQuietEndToEnd is the headline shortcut-removal proof: it opens a
// real GPT+ESP image carrying a grub.cfg, FS-patches it (removing quiet/splash,
// adding consoles), and reads the file back to confirm the change persisted
// through the FAT32 driver — no raw byte hacking, and the cmdline is genuinely
// shortened (not space-padded).
func TestPatchQuietEndToEnd(t *testing.T) {
	imgPath := buildESPImage(t, func(fs filesystem.Filesystem) {
		if err := fs.MkDir("/grub", 0o755); err != nil {
			t.Fatalf("mkdir /grub: %v", err)
		}
		if err := fs.WriteFile("/grub/grub.cfg", []byte(sampleGrubCfg), 0o644); err != nil {
			t.Fatalf("seed grub.cfg: %v", err)
		}
	})

	changed, err := PatchQuietImage(imgPath)
	if err != nil {
		t.Fatalf("PatchQuietImage: %v", err)
	}
	if !changed {
		t.Fatal("PatchQuietImage reported no change, expected quiet/splash removal")
	}

	// Re-open and read back the patched cfg.
	im, err := OpenImage(imgPath)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer im.Close()
	cfgPath, content, err := im.ReadGrubCfg()
	if err != nil {
		t.Fatalf("ReadGrubCfg: %v", err)
	}
	if cfgPath != "/grub/grub.cfg" {
		t.Errorf("cfgPath = %q, want /grub/grub.cfg", cfgPath)
	}
	if strings.Contains(content, "quiet") || strings.Contains(content, "splash") {
		t.Fatalf("patched cfg still contains quiet/splash:\n%s", content)
	}
	if !strings.Contains(content, "console=tty0") || !strings.Contains(content, "console=hvc0") {
		t.Fatalf("patched cfg missing console args:\n%s", content)
	}
	// Prove there is no space-padding artifact (the old raw hack's signature).
	if strings.Contains(content, "ro       ") || strings.Contains(content, `="     "`) {
		t.Fatalf("found space-padding artifact from raw patching:\n%s", content)
	}
}

// TestPatchQuietNoChange covers the already-clean cfg path.
func TestPatchQuietNoChange(t *testing.T) {
	clean := "set timeout=5\nmenuentry 'X' {\n\tlinux /vmlinuz root=/dev/sda2 ro console=tty0 console=hvc0\n}\n"
	imgPath := buildESPImage(t, func(fs filesystem.Filesystem) {
		if err := fs.MkDir("/grub", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := fs.WriteFile("/grub/grub.cfg", []byte(clean), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	changed, err := PatchQuietImage(imgPath)
	if err != nil {
		t.Fatalf("PatchQuietImage: %v", err)
	}
	if changed {
		t.Fatal("expected no change on already-clean cfg")
	}
}

// TestPatchQuietNoCfg covers the ErrNoGrubCfg path.
func TestPatchQuietNoCfg(t *testing.T) {
	imgPath := buildESPImage(t, nil) // no grub.cfg seeded
	if _, err := PatchQuietImage(imgPath); err != ErrNoGrubCfg {
		t.Fatalf("PatchQuietImage err = %v, want ErrNoGrubCfg", err)
	}
}

// TestPatchQuietOpenError covers OpenImage failure in the wrapper.
func TestPatchQuietOpenError(t *testing.T) {
	if _, err := PatchQuietImage("/nonexistent/disk.img"); err == nil {
		t.Fatal("expected open error")
	}
}

// TestLocateGrubCfgVendorDir exercises the /EFI/<vendor>/grub.cfg scan.
func TestLocateGrubCfgVendorDir(t *testing.T) {
	imgPath := buildESPImage(t, func(fs filesystem.Filesystem) {
		mustMkDirAll(t, fs, "/EFI/debian")
		if err := fs.WriteFile("/EFI/debian/grub.cfg", []byte("set timeout=0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	im, err := OpenImage(imgPath)
	if err != nil {
		t.Fatalf("OpenImage: %v", err)
	}
	defer im.Close()
	p, err := LocateGrubCfg(im.ESP())
	if err != nil {
		t.Fatalf("LocateGrubCfg: %v", err)
	}
	if p != "/EFI/debian/grub.cfg" {
		t.Fatalf("located %q, want /EFI/debian/grub.cfg", p)
	}
}

func mustMkDirAll(t *testing.T, fs filesystem.Filesystem, dir string) {
	t.Helper()
	if err := mkDirAll(fs, dir); err != nil {
		t.Fatalf("mkDirAll %s: %v", dir, err)
	}
}
