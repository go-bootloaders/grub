package grub

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	btrfs "github.com/go-filesystems/btrfs"
	"github.com/go-filesystems/detect"
	ext4 "github.com/go-filesystems/ext4"
	fat32 "github.com/go-filesystems/fat32"
	filesystem "github.com/go-filesystems/interface"
	"github.com/go-volumes/gpt"
)

// linuxFSImageBuilder formats a standalone filesystem image of the requested
// type and seeds it. It returns the raw image bytes for embedding in a GPT.
type linuxFSImageBuilder func(t *testing.T, path string, sizeBytes int64, seed func(fs filesystem.Filesystem))

func formatExt4(t *testing.T, path string, sizeBytes int64, seed func(fs filesystem.Filesystem)) {
	t.Helper()
	fs, err := ext4.Format(path, sizeBytes, ext4.FormatConfig{Label: "boot"})
	if err != nil {
		t.Fatalf("ext4.Format: %v", err)
	}
	if seed != nil {
		seed(fs)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("close ext4: %v", err)
	}
}

func formatBtrfs(t *testing.T, path string, sizeBytes int64, seed func(fs filesystem.Filesystem)) {
	t.Helper()
	fs, err := btrfs.Format(path, sizeBytes, btrfs.FormatConfig{Label: "boot"})
	if err != nil {
		t.Fatalf("btrfs.Format: %v", err)
	}
	if seed != nil {
		seed(fs)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("close btrfs: %v", err)
	}
}

// bootSizeBytes is the size of the Linux /boot partition image. 16 MiB is a
// multiple of 4096 (ext4) and >= 1 MiB (btrfs), and comfortably holds a
// /boot/grub tree plus small kernel/initrd fixtures.
const bootSizeBytes = 16 * 1024 * 1024

// buildESPBootImage assembles a GPT disk image with two partitions: an ESP
// (FAT32, slot 0) seeded by espSeed, and a Linux /boot partition (slot 1) of
// the filesystem produced by makeBoot and seeded by bootSeed. It returns the
// disk image path. This is a genuine on-disk GPT with two real filesystems —
// the same shape a firmware/OS would see — so OpenImage exercises the full
// locate -> probe -> mount path for /boot end to end.
func buildESPBootImage(t *testing.T, makeBoot linuxFSImageBuilder, espSeed, bootSeed func(fs filesystem.Filesystem)) string {
	t.Helper()
	dir := t.TempDir()

	// 1. Standalone FAT32 ESP.
	espPath := filepath.Join(dir, "esp.img")
	espFS, err := fat32.Format(espPath, espSizeBytes, fat32.FormatConfig{Label: "EFI"})
	if err != nil {
		t.Fatalf("fat32.Format: %v", err)
	}
	if espSeed != nil {
		espSeed(espFS)
	}
	if err := espFS.Close(); err != nil {
		t.Fatalf("close esp: %v", err)
	}
	espBytes, err := os.ReadFile(espPath)
	if err != nil {
		t.Fatalf("read esp image: %v", err)
	}

	// 2. Standalone Linux /boot filesystem.
	bootPath := filepath.Join(dir, "boot.img")
	makeBoot(t, bootPath, bootSizeBytes, bootSeed)
	bootBytes, err := os.ReadFile(bootPath)
	if err != nil {
		t.Fatalf("read boot image: %v", err)
	}

	// 3. Assemble the GPT: MBR + primary header + two entries + the two FS
	//    payloads back to back, both LBA-aligned.
	espStart := int64(espStartLBA) * testSectorSize
	bootStart := espStart + int64(len(espBytes))
	// Align bootStart up to a sector boundary (it already is, len(espBytes) is a
	// whole number of sectors, but keep this robust).
	if rem := bootStart % testSectorSize; rem != 0 {
		bootStart += testSectorSize - rem
	}
	deviceSize := bootStart + int64(len(bootBytes)) + 64*testSectorSize
	img := make([]byte, deviceSize)

	// Protective MBR.
	img[510] = 0x55
	img[511] = 0xAA
	img[446+4] = 0xEE
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

	// Entry 0: ESP.
	espEndLBA := uint64(espStart/testSectorSize) + uint64(len(espBytes))/testSectorSize - 1
	espEntry := make([]byte, entrySize)
	copy(espEntry[0:16], gpt.EFISystemPartitionGUID[:])
	for i := 0; i < 16; i++ {
		espEntry[16+i] = byte(0xA0 + i)
	}
	binary.LittleEndian.PutUint64(espEntry[32:], uint64(espStart/testSectorSize))
	binary.LittleEndian.PutUint64(espEntry[40:], espEndLBA)
	copy(img[int64(entryLBA)*testSectorSize:], espEntry)

	// Entry 1: Linux /boot.
	bootStartLBA := uint64(bootStart / testSectorSize)
	bootEndLBA := bootStartLBA + uint64(len(bootBytes))/testSectorSize - 1
	bootEntry := make([]byte, entrySize)
	copy(bootEntry[0:16], gpt.LinuxFilesystemGUID[:])
	for i := 0; i < 16; i++ {
		bootEntry[16+i] = byte(0xB0 + i)
	}
	binary.LittleEndian.PutUint64(bootEntry[32:], bootStartLBA)
	binary.LittleEndian.PutUint64(bootEntry[40:], bootEndLBA)
	copy(img[int64(entryLBA)*testSectorSize+entrySize:], bootEntry)

	// 4. Payloads.
	copy(img[espStart:], espBytes)
	copy(img[bootStart:], bootBytes)

	imgPath := filepath.Join(dir, "disk.img")
	if err := os.WriteFile(imgPath, img, 0o644); err != nil {
		t.Fatalf("write disk image: %v", err)
	}
	return imgPath
}

// seedBootGrub seeds a /boot filesystem with a grub.cfg, a vmlinuz and a
// matching initrd, the canonical Debian-style layout.
func seedBootGrub(t *testing.T) func(fs filesystem.Filesystem) {
	return func(fs filesystem.Filesystem) {
		for _, d := range []string{"/boot", "/boot/grub"} {
			if err := fs.MkDir(d, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", d, err)
			}
		}
		cfg := "menuentry 'Debian' {\n\tlinux /boot/vmlinuz-6.1.0-18-amd64 root=/dev/sda2 ro quiet splash\n\tinitrd /boot/initrd.img-6.1.0-18-amd64\n}\n"
		if err := fs.WriteFile("/boot/grub/grub.cfg", []byte(cfg), 0o644); err != nil {
			t.Fatalf("write grub.cfg: %v", err)
		}
		if err := fs.WriteFile("/boot/vmlinuz-6.1.0-18-amd64", []byte("KERNEL"), 0o644); err != nil {
			t.Fatalf("write vmlinuz: %v", err)
		}
		if err := fs.WriteFile("/boot/initrd.img-6.1.0-18-amd64", []byte("INITRD"), 0o644); err != nil {
			t.Fatalf("write initrd: %v", err)
		}
	}
}

// --- end-to-end ext4 /boot ------------------------------------------------

func TestOpenImageMountsExt4Boot(t *testing.T) {
	imgPath := buildESPBootImage(t, formatExt4, nil, seedBootGrub(t))

	im, err := OpenImage(imgPath)
	if err != nil {
		t.Fatalf("OpenImage: %v", err)
	}
	defer im.Close()

	if !im.HasBoot() {
		t.Fatal("HasBoot() = false, want true")
	}
	if im.BootType() != detect.Ext4 {
		t.Fatalf("BootType() = %q, want ext4", im.BootType())
	}
	if im.Boot() == nil {
		t.Fatal("Boot() = nil")
	}
	p, ok, err := im.BootPartition()
	if err != nil || !ok {
		t.Fatalf("BootPartition() = (%v, %v, %v)", p, ok, err)
	}
	if p.Index != 1 {
		t.Fatalf("/boot partition Index = %d, want 1", p.Index)
	}

	// grub.cfg discovery on /boot.
	cfgPath, content, err := im.ReadGrubCfgOnBoot()
	if err != nil {
		t.Fatalf("ReadGrubCfgOnBoot: %v", err)
	}
	if cfgPath != "/boot/grub/grub.cfg" {
		t.Fatalf("cfg path = %q, want /boot/grub/grub.cfg", cfgPath)
	}
	if !strings.Contains(content, "vmlinuz-6.1.0-18-amd64") {
		t.Fatalf("grub.cfg content unexpected: %q", content)
	}

	// Kernel discovery on /boot.
	kernels, err := DiscoverKernels(im.Boot())
	if err != nil {
		t.Fatalf("DiscoverKernels: %v", err)
	}
	if len(kernels) != 1 {
		t.Fatalf("discovered %d kernels, want 1", len(kernels))
	}
	k := kernels[0]
	if k.KernelPath != "/boot/vmlinuz-6.1.0-18-amd64" {
		t.Fatalf("kernel path = %q", k.KernelPath)
	}
	if k.InitrdPath != "/boot/initrd.img-6.1.0-18-amd64" {
		t.Fatalf("initrd path = %q", k.InitrdPath)
	}
	if k.Version != "6.1.0-18-amd64" {
		t.Fatalf("version = %q", k.Version)
	}
}

func TestPatchQuietOnExt4Boot(t *testing.T) {
	imgPath := buildESPBootImage(t, formatExt4, nil, seedBootGrub(t))

	im, err := OpenImage(imgPath)
	if err != nil {
		t.Fatalf("OpenImage: %v", err)
	}
	changed, err := im.PatchQuietOnBoot()
	if err != nil {
		t.Fatalf("PatchQuietOnBoot: %v", err)
	}
	if !changed {
		t.Fatal("PatchQuietOnBoot reported no change")
	}
	// Idempotent second pass.
	again, err := im.PatchQuietOnBoot()
	if err != nil {
		t.Fatalf("PatchQuietOnBoot 2: %v", err)
	}
	if again {
		t.Fatal("second PatchQuietOnBoot changed an already-patched cfg")
	}
	if err := im.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Re-open and confirm the write persisted to the ext4 partition in the
	// disk image: quiet/splash gone, consoles present.
	im2, err := OpenImage(imgPath)
	if err != nil {
		t.Fatalf("re-OpenImage: %v", err)
	}
	defer im2.Close()
	_, content, err := im2.ReadGrubCfgOnBoot()
	if err != nil {
		t.Fatalf("re-read grub.cfg: %v", err)
	}
	if strings.Contains(content, "quiet") || strings.Contains(content, "splash") {
		t.Fatalf("quiet/splash not stripped persistently: %q", content)
	}
	if !strings.Contains(content, "console=hvc0") {
		t.Fatalf("console not added persistently: %q", content)
	}
}

func TestMkConfigOnExt4Boot(t *testing.T) {
	// Seed kernels but no grub.cfg, so MkConfig must create one.
	seed := func(fs filesystem.Filesystem) {
		if err := fs.MkDir("/boot", 0o755); err != nil {
			t.Fatalf("mkdir /boot: %v", err)
		}
		if err := fs.WriteFile("/boot/vmlinuz-6.5.0", []byte("K"), 0o644); err != nil {
			t.Fatalf("write kernel: %v", err)
		}
		if err := fs.WriteFile("/boot/initrd.img-6.5.0", []byte("I"), 0o644); err != nil {
			t.Fatalf("write initrd: %v", err)
		}
	}
	imgPath := buildESPBootImage(t, formatExt4, nil, seed)

	im, err := OpenImage(imgPath)
	if err != nil {
		t.Fatalf("OpenImage: %v", err)
	}
	cfgPath, n, err := im.MkConfigOnBoot(MkConfigOptions{Distributor: "Debian"})
	if err != nil {
		t.Fatalf("MkConfigOnBoot: %v", err)
	}
	if n != 1 {
		t.Fatalf("entries = %d, want 1", n)
	}
	if cfgPath != "/grub/grub.cfg" {
		t.Fatalf("cfg path = %q, want /grub/grub.cfg", cfgPath)
	}
	if err := im.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	im2, err := OpenImage(imgPath)
	if err != nil {
		t.Fatalf("re-OpenImage: %v", err)
	}
	defer im2.Close()
	_, content, err := im2.ReadGrubCfgOnBoot()
	if err != nil {
		t.Fatalf("re-read generated cfg: %v", err)
	}
	if !strings.Contains(content, "vmlinuz-6.5.0") || !strings.Contains(content, "Debian") {
		t.Fatalf("generated cfg unexpected: %q", content)
	}
}

func TestMeasureBootOnExt4Boot(t *testing.T) {
	imgPath := buildESPBootImage(t, formatExt4, nil, seedBootGrub(t))
	im, err := OpenImage(imgPath)
	if err != nil {
		t.Fatalf("OpenImage: %v", err)
	}
	defer im.Close()
	n, err := im.MeasureBootOnBoot(NewMeasurer(&fakeCaller{}))
	if err != nil {
		t.Fatalf("MeasureBootOnBoot: %v", err)
	}
	// 1 config + 1 kernel + 1 initrd.
	if n != 3 {
		t.Fatalf("measurements = %d, want 3", n)
	}
}

func TestPatchQuietImageCoversBoth(t *testing.T) {
	// ESP carries an /EFI/debian/grub.cfg, /boot carries /boot/grub/grub.cfg;
	// PatchQuietImage must patch both in one call.
	espSeed := func(fs filesystem.Filesystem) {
		for _, d := range []string{"/EFI", "/EFI/debian"} {
			if err := fs.MkDir(d, 0o755); err != nil {
				t.Fatalf("esp mkdir %s: %v", d, err)
			}
		}
		cfg := "menuentry 'x' {\n\tlinux /vmlinuz root=/dev/sda2 ro quiet\n}\n"
		if err := fs.WriteFile("/EFI/debian/grub.cfg", []byte(cfg), 0o644); err != nil {
			t.Fatalf("esp write cfg: %v", err)
		}
	}
	imgPath := buildESPBootImage(t, formatExt4, espSeed, seedBootGrub(t))

	changed, err := PatchQuietImage(imgPath)
	if err != nil {
		t.Fatalf("PatchQuietImage: %v", err)
	}
	if !changed {
		t.Fatal("PatchQuietImage reported no change")
	}

	im, err := OpenImage(imgPath)
	if err != nil {
		t.Fatalf("OpenImage: %v", err)
	}
	defer im.Close()
	_, espCfg, err := im.ReadGrubCfg()
	if err != nil {
		t.Fatalf("read esp cfg: %v", err)
	}
	if strings.Contains(espCfg, "quiet") {
		t.Fatalf("esp quiet not stripped: %q", espCfg)
	}
	_, bootCfg, err := im.ReadGrubCfgOnBoot()
	if err != nil {
		t.Fatalf("read boot cfg: %v", err)
	}
	if strings.Contains(bootCfg, "quiet") || strings.Contains(bootCfg, "splash") {
		t.Fatalf("boot quiet/splash not stripped: %q", bootCfg)
	}
}

// --- end-to-end btrfs /boot -----------------------------------------------

func TestOpenImageMountsBtrfsBoot(t *testing.T) {
	imgPath := buildESPBootImage(t, formatBtrfs, nil, seedBootGrub(t))

	im, err := OpenImage(imgPath)
	if err != nil {
		t.Fatalf("OpenImage (btrfs): %v", err)
	}
	defer im.Close()

	if im.BootType() != detect.Btrfs {
		t.Fatalf("BootType() = %q, want btrfs", im.BootType())
	}
	cfgPath, _, err := im.ReadGrubCfgOnBoot()
	if err != nil {
		t.Fatalf("ReadGrubCfgOnBoot (btrfs): %v", err)
	}
	if cfgPath != "/boot/grub/grub.cfg" {
		t.Fatalf("btrfs cfg path = %q", cfgPath)
	}
	kernels, err := DiscoverKernels(im.Boot())
	if err != nil {
		t.Fatalf("DiscoverKernels (btrfs): %v", err)
	}
	if len(kernels) != 1 || kernels[0].KernelPath != "/boot/vmlinuz-6.1.0-18-amd64" {
		t.Fatalf("btrfs kernel discovery: %+v", kernels)
	}
}

// --- unsupported / error branches -----------------------------------------

func TestOpenImageUnsupportedBootFS(t *testing.T) {
	imgPath := buildESPBootImage(t, formatExt4, nil, seedBootGrub(t))
	// Make detect report the /boot partition as XFS (unsupported) while the ESP
	// still probes as FAT32.
	defer swap(&detectType, func(r io.ReaderAt, size int64) (detect.Type, error) {
		// First call probes the ESP, later calls probe /boot. Distinguish by
		// reading the FAT32 label region; simpler: dispatch on whether the real
		// detector says FAT32.
		typ, err := detect.Detect(r, size)
		if err == nil && typ == detect.FAT32 {
			return detect.FAT32, nil
		}
		return detect.XFS, nil
	})()
	_, err := OpenImage(imgPath)
	if !errors.Is(err, ErrUnsupportedBootFS) {
		t.Fatalf("err = %v, want ErrUnsupportedBootFS", err)
	}
}

func TestOpenImageBootProbeError(t *testing.T) {
	imgPath := buildESPBootImage(t, formatExt4, nil, seedBootGrub(t))
	defer swap(&detectType, func(r io.ReaderAt, size int64) (detect.Type, error) {
		typ, err := detect.Detect(r, size)
		if err == nil && typ == detect.FAT32 {
			return detect.FAT32, nil
		}
		return "", errors.New("boot probe boom")
	})()
	if _, err := OpenImage(imgPath); err == nil {
		t.Fatal("expected boot probe error")
	}
}

func TestOpenImageBootMountError(t *testing.T) {
	imgPath := buildESPBootImage(t, formatExt4, nil, seedBootGrub(t))
	defer swap(&ext4Open, func(string, int) (filesystem.Filesystem, error) {
		return nil, errors.New("ext4 mount boom")
	})()
	if _, err := OpenImage(imgPath); err == nil {
		t.Fatal("expected ext4 mount error")
	}
}

func TestBootMethodsNoBoot(t *testing.T) {
	// ESP-only image: the *OnBoot methods return ErrNoBoot.
	imgPath := buildESPImage(t, nil)
	im, err := OpenImage(imgPath)
	if err != nil {
		t.Fatalf("OpenImage: %v", err)
	}
	defer im.Close()
	if im.HasBoot() {
		t.Fatal("HasBoot() = true on ESP-only image")
	}
	if _, _, err := im.ReadGrubCfgOnBoot(); !errors.Is(err, ErrNoBoot) {
		t.Fatalf("ReadGrubCfgOnBoot err = %v, want ErrNoBoot", err)
	}
	if err := im.WriteGrubCfgOnBoot("/x", "y"); !errors.Is(err, ErrNoBoot) {
		t.Fatalf("WriteGrubCfgOnBoot err = %v, want ErrNoBoot", err)
	}
	if _, err := im.PatchQuietOnBoot(); !errors.Is(err, ErrNoBoot) {
		t.Fatalf("PatchQuietOnBoot err = %v, want ErrNoBoot", err)
	}
	if _, _, err := im.MkConfigOnBoot(MkConfigOptions{}); !errors.Is(err, ErrNoBoot) {
		t.Fatalf("MkConfigOnBoot err = %v, want ErrNoBoot", err)
	}
	if _, err := im.MeasureBootOnBoot(NewMeasurer(&fakeCaller{})); !errors.Is(err, ErrNoBoot) {
		t.Fatalf("MeasureBootOnBoot err = %v, want ErrNoBoot", err)
	}
}

// TestCloseBootError covers the /boot Close error branch (boot close fails;
// the error is surfaced).
func TestCloseBootError(t *testing.T) {
	boot := newErrFS()
	boot.closeErr = errors.New("boot close boom")
	im := &Image{path: "mem", esp: newErrFS(), boot: boot, hasBoot: true}
	if err := im.Close(); err == nil {
		t.Fatal("expected boot close error")
	}
}

// TestBootPartitionReprobe covers the BootPartition fallback that re-probes the
// disk when no /boot is mounted but a Linux partition is physically present
// (e.g. an Image constructed without going through mountBoot).
func TestBootPartitionReprobe(t *testing.T) {
	imgPath := buildESPBootImage(t, formatExt4, nil, seedBootGrub(t))
	st, err := os.Stat(imgPath)
	if err != nil {
		t.Fatal(err)
	}
	im := &Image{path: imgPath, size: st.Size()} // hasBoot=false on purpose
	p, ok, err := im.BootPartition()
	if err != nil || !ok {
		t.Fatalf("BootPartition reprobe = (%v, %v, %v)", p, ok, err)
	}
	if p.Index != 1 {
		t.Fatalf("reprobed Index = %d, want 1", p.Index)
	}
}

func TestWriteGrubCfgOnBootPersists(t *testing.T) {
	imgPath := buildESPBootImage(t, formatExt4, nil, seedBootGrub(t))
	im, err := OpenImage(imgPath)
	if err != nil {
		t.Fatalf("OpenImage: %v", err)
	}
	if err := im.WriteGrubCfgOnBoot("/boot/grub/grub.cfg", "set timeout=9\n"); err != nil {
		t.Fatalf("WriteGrubCfgOnBoot: %v", err)
	}
	if err := im.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	im2, err := OpenImage(imgPath)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer im2.Close()
	_, content, err := im2.ReadGrubCfgOnBoot()
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if content != "set timeout=9\n" {
		t.Fatalf("written content not persisted: %q", content)
	}
}
