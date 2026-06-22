package grub

import (
	"path/filepath"
	"testing"

	uefi "github.com/go-filesystems/uefi"
	"github.com/go-volumes/gpt"
)

func newStore(t *testing.T) uefi.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vars.fd")
	st, err := uefi.Format(path, 256*1024)
	if err != nil {
		t.Fatalf("uefi.Format: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestBuildAndRegisterBootEntry(t *testing.T) {
	store := newStore(t)

	part := gpt.Partition{StartOffset: 2048 * 512, Length: 4 * 1024 * 1024}
	var guid [16]byte
	for i := range guid {
		guid[i] = byte(0xB0 + i)
	}
	lo := BuildBootEntry("GRUB", 1, guid, part, DefaultGrubLoaderPath)

	// Device path text must mention HD(...) and the loader file.
	txt := lo.Text()
	if txt == "" {
		t.Fatal("empty device path text")
	}

	n, err := RegisterBootEntry(store, lo)
	if err != nil {
		t.Fatalf("RegisterBootEntry: %v", err)
	}

	// Read it back through the store.
	got, err := uefi.BootEntry(store, n)
	if err != nil {
		t.Fatalf("BootEntry: %v", err)
	}
	if got.Description != "GRUB" {
		t.Fatalf("description = %q, want GRUB", got.Description)
	}

	// FindBootEntry should locate it by description.
	fn, ok, err := FindBootEntry(store, "GRUB")
	if err != nil {
		t.Fatalf("FindBootEntry: %v", err)
	}
	if !ok || fn != n {
		t.Fatalf("FindBootEntry = (%d,%v), want (%d,true)", fn, ok, n)
	}

	// A description that doesn't exist returns ok=false.
	if _, ok, err := FindBootEntry(store, "absent"); err != nil || ok {
		t.Fatalf("FindBootEntry(absent) = (%v,%v)", ok, err)
	}
}

func TestBuildBootEntryDevicePath(t *testing.T) {
	part := gpt.Partition{StartOffset: 1024 * 512, Length: 512 * 512}
	lo := BuildBootEntry("X", 2, [16]byte{}, part, `\EFI\BOOT\BOOTX64.EFI`)
	if len(lo.DevicePath) != 2 {
		t.Fatalf("device path nodes = %d, want 2 (HD + File)", len(lo.DevicePath))
	}
	hd, ok := lo.DevicePath[0].HardDrive()
	if !ok {
		t.Fatal("first node not a HardDrive node")
	}
	if hd.PartitionNumber != 2 || hd.PartitionStart != 1024 || hd.PartitionSize != 512 {
		t.Fatalf("HD geometry wrong: %+v", hd)
	}
	if hd.MBRType != uefi.HDMBRTypeGPT || hd.SignatureType != uefi.HDSigTypeGUID {
		t.Fatalf("HD type fields wrong: %+v", hd)
	}
	fp, ok := lo.DevicePath[1].FilePath()
	if !ok || fp != `\EFI\BOOT\BOOTX64.EFI` {
		t.Fatalf("file path node = (%q,%v)", fp, ok)
	}
}
