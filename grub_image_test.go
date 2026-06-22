package grub

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	fat32 "github.com/go-filesystems/fat32"
	filesystem "github.com/go-filesystems/interface"
	"github.com/go-volumes/gpt"
)

// --- GPT+ESP image construction -------------------------------------------

const (
	testSectorSize = 512
	// espStartLBA places the ESP after the GPT area (LBA 0 MBR, 1 header,
	// 2..33 entry array of 128*4=512 bytes => 1 sector here, but reserve 34).
	espStartLBA = 2048
	// espSizeBytes is a 4 MiB FAT32 volume (the fat32 driver's minimum).
	espSizeBytes = 4 * 1024 * 1024
)

// buildESPImage formats a 4 MiB FAT32 volume, seeds it via seed, then wraps it
// in a minimal GPT (protective MBR + primary header + one ESP entry) and
// writes the whole image to a temp file. It returns the image path. The GPT
// reader in go-volumes/gpt does not verify header/entry CRCs, so a minimal but
// structurally-correct layout suffices — this is a real on-disk GPT a firmware
// would parse for the partition table, with a genuine FAT32 ESP inside it.
func buildESPImage(t *testing.T, seed func(fs filesystem.Filesystem)) string {
	t.Helper()
	dir := t.TempDir()

	// 1. Format a standalone FAT32 image and seed it.
	espPath := filepath.Join(dir, "esp.img")
	fs, err := fat32.Format(espPath, espSizeBytes, fat32.FormatConfig{Label: "EFI"})
	if err != nil {
		t.Fatalf("fat32.Format: %v", err)
	}
	if seed != nil {
		seed(fs)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("close formatted esp: %v", err)
	}
	espBytes, err := os.ReadFile(espPath)
	if err != nil {
		t.Fatalf("read esp image: %v", err)
	}

	// 2. Assemble the GPT disk image.
	espStart := int64(espStartLBA) * testSectorSize
	deviceSize := espStart + int64(len(espBytes)) + 64*testSectorSize // tail slack for backup GPT
	img := make([]byte, deviceSize)

	// Protective MBR.
	img[510] = 0x55
	img[511] = 0xAA
	img[446+4] = 0xEE // type 0xEE protective
	binary.LittleEndian.PutUint32(img[446+8:], 1)
	binary.LittleEndian.PutUint32(img[446+12:], 0xFFFFFFFF)

	// Primary GPT header at LBA 1.
	const entryLBA = 2
	const numParts = 4
	const entrySize = 128
	hoff := int64(1) * testSectorSize
	copy(img[hoff:], []byte("EFI PART"))
	binary.LittleEndian.PutUint64(img[hoff+72:], entryLBA)
	binary.LittleEndian.PutUint32(img[hoff+80:], numParts)
	binary.LittleEndian.PutUint32(img[hoff+84:], entrySize)

	// One ESP entry at entryLBA.
	espEndLBA := uint64(espStartLBA) + uint64(len(espBytes))/testSectorSize - 1
	entry := make([]byte, entrySize)
	copy(entry[0:16], gpt.EFISystemPartitionGUID[:])
	// Unique partition GUID (bytes 16:32) — arbitrary but stable.
	for i := 0; i < 16; i++ {
		entry[16+i] = byte(0xA0 + i)
	}
	binary.LittleEndian.PutUint64(entry[32:], espStartLBA)
	binary.LittleEndian.PutUint64(entry[40:], espEndLBA)
	toff := int64(entryLBA) * testSectorSize
	copy(img[toff:], entry)

	// 3. The FAT32 ESP bytes at the partition start.
	copy(img[espStart:], espBytes)

	imgPath := filepath.Join(dir, "disk.img")
	if err := os.WriteFile(imgPath, img, 0o644); err != nil {
		t.Fatalf("write disk image: %v", err)
	}
	return imgPath
}

// --- OpenImage / ESP round-trip -------------------------------------------

func TestOpenImageLocatesAndMountsESP(t *testing.T) {
	imgPath := buildESPImage(t, func(fs filesystem.Filesystem) {
		if err := fs.WriteFile("/marker.txt", []byte("hello-esp"), 0o644); err != nil {
			t.Fatalf("seed marker: %v", err)
		}
	})

	im, err := OpenImage(imgPath)
	if err != nil {
		t.Fatalf("OpenImage: %v", err)
	}
	defer im.Close()

	if im.Path() != imgPath {
		t.Errorf("Path() = %q, want %q", im.Path(), imgPath)
	}
	if im.Size() <= 0 {
		t.Errorf("Size() = %d, want > 0", im.Size())
	}
	if im.ESPPartition().StartOffset != int64(espStartLBA)*testSectorSize {
		t.Errorf("ESP start = %d, want %d", im.ESPPartition().StartOffset, int64(espStartLBA)*testSectorSize)
	}

	data, err := im.ESP().ReadFile("/marker.txt")
	if err != nil {
		t.Fatalf("ReadFile marker via mounted ESP: %v", err)
	}
	if string(data) != "hello-esp" {
		t.Fatalf("marker = %q, want hello-esp", data)
	}
}

func TestOpenImageNoESP(t *testing.T) {
	// A bare disk with a protective MBR but no ESP entry.
	dir := t.TempDir()
	img := make([]byte, 8*1024*1024)
	img[510] = 0x55
	img[511] = 0xAA
	copy(img[testSectorSize:], []byte("EFI PART"))
	binary.LittleEndian.PutUint64(img[testSectorSize+72:], 2)
	binary.LittleEndian.PutUint32(img[testSectorSize+80:], 4)
	binary.LittleEndian.PutUint32(img[testSectorSize+84:], 128)
	p := filepath.Join(dir, "noesp.img")
	if err := os.WriteFile(p, img, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenImage(p); err != ErrNoESP {
		t.Fatalf("OpenImage no-ESP err = %v, want ErrNoESP", err)
	}
}

func TestOpenImageMissingFile(t *testing.T) {
	if _, err := OpenImage(filepath.Join(t.TempDir(), "nope.img")); err == nil {
		t.Fatal("expected open error for missing file")
	}
}

func TestBootPartitionAbsent(t *testing.T) {
	imgPath := buildESPImage(t, nil)
	im, err := OpenImage(imgPath)
	if err != nil {
		t.Fatalf("OpenImage: %v", err)
	}
	defer im.Close()
	_, ok, err := im.BootPartition()
	if err != nil {
		t.Fatalf("BootPartition err: %v", err)
	}
	if ok {
		t.Fatal("expected no /boot partition in ESP-only image")
	}
}
