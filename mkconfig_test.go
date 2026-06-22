package grub

import (
	"strings"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// seedKernels drops a versioned kernel+initrd pair and an un-versioned pair
// under /boot of the ESP.
func seedKernels(t *testing.T, fs filesystem.Filesystem) {
	t.Helper()
	if err := fs.MkDir("/boot", 0o755); err != nil {
		t.Fatalf("mkdir /boot: %v", err)
	}
	files := map[string]string{
		"/boot/vmlinuz-6.1.0-18-amd64":    "KERNEL-A",
		"/boot/initrd.img-6.1.0-18-amd64": "INITRD-A",
		"/boot/vmlinuz-5.10.0-9-amd64":    "KERNEL-B",
		"/boot/initrd.img-5.10.0-9-amd64": "INITRD-B",
	}
	for p, c := range files {
		if err := fs.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}
}

func TestMkConfigEndToEnd(t *testing.T) {
	imgPath := buildESPImage(t, func(fs filesystem.Filesystem) {
		seedKernels(t, fs)
	})
	im, err := OpenImage(imgPath)
	if err != nil {
		t.Fatalf("OpenImage: %v", err)
	}
	defer im.Close()

	cfgPath, n, err := im.MkConfig(MkConfigOptions{Distributor: "Debian GNU/Linux"})
	if err != nil {
		t.Fatalf("MkConfig: %v", err)
	}
	if n != 2 {
		t.Fatalf("MkConfig entries = %d, want 2", n)
	}
	if cfgPath != "/grub/grub.cfg" {
		t.Fatalf("MkConfig wrote %q, want /grub/grub.cfg", cfgPath)
	}

	_, content, err := im.ReadGrubCfg()
	if err != nil {
		t.Fatalf("ReadGrubCfg: %v", err)
	}
	// Validate the generated config structurally.
	for _, want := range []string{
		"set default=0",
		"set timeout=5",
		"insmod part_gpt",
		"menuentry 'Debian GNU/Linux (6.1.0-18-amd64)' {",
		"linux /boot/vmlinuz-6.1.0-18-amd64 ro console=tty0 console=hvc0",
		"initrd /boot/initrd.img-6.1.0-18-amd64",
		"menuentry 'Debian GNU/Linux (5.10.0-9-amd64)' {",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("generated cfg missing %q:\n%s", want, content)
		}
	}
	// Newest kernel first (6.1.0 before 5.10.0).
	if strings.Index(content, "6.1.0-18") > strings.Index(content, "5.10.0-9") {
		t.Fatalf("kernels not newest-first:\n%s", content)
	}
}

func TestMkConfigNoKernels(t *testing.T) {
	imgPath := buildESPImage(t, nil)
	im, err := OpenImage(imgPath)
	if err != nil {
		t.Fatalf("OpenImage: %v", err)
	}
	defer im.Close()
	if _, _, err := im.MkConfig(MkConfigOptions{}); err == nil {
		t.Fatal("expected error when no kernels present")
	}
}

func TestMkConfigUsesExistingCfgPath(t *testing.T) {
	imgPath := buildESPImage(t, func(fs filesystem.Filesystem) {
		mustMkDirAll(t, fs, "/EFI/debian")
		if err := fs.WriteFile("/EFI/debian/grub.cfg", []byte("old\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		seedKernels(t, fs)
	})
	im, err := OpenImage(imgPath)
	if err != nil {
		t.Fatalf("OpenImage: %v", err)
	}
	defer im.Close()
	cfgPath, _, err := im.MkConfig(MkConfigOptions{})
	if err != nil {
		t.Fatalf("MkConfig: %v", err)
	}
	if cfgPath != "/EFI/debian/grub.cfg" {
		t.Fatalf("MkConfig should reuse existing cfg path, got %q", cfgPath)
	}
}

func TestDiscoverKernelsUnversioned(t *testing.T) {
	imgPath := buildESPImage(t, func(fs filesystem.Filesystem) {
		if err := fs.MkDir("/boot", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := fs.WriteFile("/boot/vmlinuz", []byte("K"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := fs.WriteFile("/boot/initrd.img", []byte("I"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	im, err := OpenImage(imgPath)
	if err != nil {
		t.Fatalf("OpenImage: %v", err)
	}
	defer im.Close()
	ks, err := DiscoverKernels(im.ESP())
	if err != nil {
		t.Fatalf("DiscoverKernels: %v", err)
	}
	if len(ks) != 1 {
		t.Fatalf("got %d kernels, want 1: %+v", len(ks), ks)
	}
	if ks[0].Version != "" || ks[0].KernelPath != "/boot/vmlinuz" || ks[0].InitrdPath != "/boot/initrd.img" {
		t.Fatalf("unversioned kernel mismatch: %+v", ks[0])
	}
}

// TestGenerateConfigPure unit-tests the renderer without an image.
func TestGenerateConfigPure(t *testing.T) {
	cfg := GenerateConfig([]Kernel{
		{Version: "1.0", KernelPath: "/boot/vmlinuz-1.0", InitrdPath: "/boot/initrd-1.0"},
		{Version: "", KernelPath: "/boot/vmlinuz"},
	}, MkConfigOptions{Default: 1, Timeout: 9, Distributor: "Test'OS", Cmdline: "ro"})

	if !strings.Contains(cfg, "set default=1") || !strings.Contains(cfg, "set timeout=9") {
		t.Fatalf("header wrong:\n%s", cfg)
	}
	// Single-quote escaping in the distributor name.
	if !strings.Contains(cfg, `Test'\''OS`) {
		t.Fatalf("single-quote not escaped:\n%s", cfg)
	}
	// The un-versioned kernel has no initrd line.
	if strings.Count(cfg, "initrd ") != 1 {
		t.Fatalf("expected exactly one initrd line:\n%s", cfg)
	}
}

func TestKernelVersionAndInitrd(t *testing.T) {
	if got := kernelVersion("vmlinuz-6.1.0"); got != "6.1.0" {
		t.Errorf("kernelVersion = %q", got)
	}
	if got := kernelVersion("vmlinuz"); got != "" {
		t.Errorf("kernelVersion(unversioned) = %q", got)
	}
	if !isKernelName("vmlinuz-x") || !isKernelName("kernel") || isKernelName("README") {
		t.Error("isKernelName misclassified")
	}
	if len(initrdCandidates("")) == 0 || len(initrdCandidates("1.0")) == 0 {
		t.Error("initrdCandidates empty")
	}
}
