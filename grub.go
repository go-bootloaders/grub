// Package grub is a pure-Go (CGO=0) GRUB administration toolkit for disk
// images. It is a production consumer of the go-volumes / go-filesystems /
// go-tpm2 stack:
//
//   - go-volumes/gpt locates the EFI System Partition (and an optional Linux
//     /boot partition) inside a GPT disk image.
//   - go-filesystems/detect (+ its fat32reg adapter) mounts the partition's
//     filesystem behind the common filesystem.Filesystem interface.
//   - go-filesystems/uefi registers a GRUB UEFI boot entry (Boot####/BootOrder).
//   - go-tpm2/efitcg2 measures the grub.cfg + referenced kernel/initrd into the
//     TPM PCRs for measured boot (optional, behind an injected Caller).
//
// All grub.cfg editing is performed through the mounted filesystem's
// ReadFile/WriteFile — there is no raw same-length byte patching of the disk
// image. (RawReplaceAll survives only as a documented, deliberately-fenced
// low-level fallback; see raw.go.)
package grub

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"

	"github.com/go-filesystems/detect"
	_ "github.com/go-filesystems/detect/fat32reg" // register the FAT32 opener (type probe)
	fat32 "github.com/go-filesystems/fat32"
	filesystem "github.com/go-filesystems/interface"
	"github.com/go-volumes/gpt"
)

// candidateCfgPaths lists, in priority order, where grub.cfg conventionally
// lives on an ESP or a /boot filesystem. The first one that exists wins.
var candidateCfgPaths = []string{
	"/grub/grub.cfg",
	"/boot/grub/grub.cfg",
	"/grub2/grub.cfg",
	"/boot/grub2/grub.cfg",
	"/EFI/grub/grub.cfg",
}

// efiRoot is scanned for /EFI/<vendor>/grub.cfg when the fixed candidate list
// above misses (distros drop grub.cfg under /EFI/<distro>/grub.cfg, e.g.
// /EFI/debian/grub.cfg).
const efiRoot = "/EFI"

// ErrNoESP is returned when the image has no EFI System Partition.
var ErrNoESP = errors.New("grub: no EFI System Partition found in image")

// ErrNoGrubCfg is returned when no grub.cfg can be located on a mounted FS.
var ErrNoGrubCfg = errors.New("grub: no grub.cfg found on filesystem")

// Image is a mounted GRUB-administration handle over a GPT disk image. It owns
// the opened backing file and the mounted ESP filesystem; call Close to
// release both. The optional Linux /boot filesystem is mounted lazily and only
// when it resolves cleanly (see OpenImage).
type Image struct {
	path string
	size int64

	espPart gpt.Partition
	esp     filesystem.Filesystem
}

// Seams over os/io so each error branch is exercisable in tests.
var (
	osOpenFile = os.OpenFile
	gptByType  = gpt.ByType
	newSection = io.NewSectionReader
	detectType = detect.Detect
	fat32Open  = fat32.Open
)

// OpenImage opens a GPT disk image read/write, locates the EFI System
// Partition, and mounts its FAT32 filesystem for read/write. The returned Image
// exposes the mounted ESP via ESP(); callers must Close it.
//
// The ESP is mounted writably through the go-filesystems/fat32 driver, which
// opens the partition in place inside the disk image (partition selected by its
// GPT index) so WriteFile changes persist to the underlying image. The FS type
// is first verified as FAT32 via go-filesystems/detect; a non-FAT32 ESP is
// rejected rather than mis-mounted.
//
// The Linux /boot partition (LinuxFilesystemGUID) is intentionally NOT mounted
// here: that partition is typically ext4, whose driver module currently drags
// in the go-diskimages v0.0.0+replace tangle that does not resolve in an
// isolated downstream build. The ESP/FAT32 path is fully supported and clean;
// mounting /boot-ext4 is a documented follow-up gated on the org-wide
// pseudo-version migration. Use BootPartition() to inspect (not mount) it.
func OpenImage(imagePath string) (*Image, error) {
	f, err := osOpenFile(imagePath, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("grub: open image: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("grub: stat image: %w", err)
	}
	size := st.Size()

	esp, err := gptByType(f, size, gpt.EFISystemPartitionGUID)
	if err != nil {
		f.Close()
		if errors.Is(err, gpt.ErrNotFound) {
			return nil, ErrNoESP
		}
		return nil, fmt.Errorf("grub: locate ESP: %w", err)
	}

	// Verify the ESP is FAT32 before mounting it writably.
	typ, derr := detectType(newSection(f, esp.StartOffset, esp.Length), esp.Length)
	if derr != nil {
		f.Close()
		return nil, fmt.Errorf("grub: probe ESP type: %w", derr)
	}
	if typ != detect.FAT32 {
		f.Close()
		return nil, fmt.Errorf("grub: ESP is %q, not FAT32", typ)
	}
	// We are done probing through f; the writable mount re-opens the image.
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("grub: close probe handle: %w", err)
	}

	// Mount the ESP writably in place: fat32.Open selects the partition by its
	// GPT index and writes changes back to the disk image at the partition
	// offset (no temp-file staging, so edits persist).
	fs, err := fat32Open(imagePath, esp.Index)
	if err != nil {
		return nil, fmt.Errorf("grub: mount ESP read/write: %w", err)
	}

	return &Image{
		path:    imagePath,
		size:    size,
		espPart: esp,
		esp:     fs,
	}, nil
}

// ESP returns the mounted EFI System Partition filesystem.
func (im *Image) ESP() filesystem.Filesystem { return im.esp }

// ESPPartition returns the GPT geometry of the EFI System Partition.
func (im *Image) ESPPartition() gpt.Partition { return im.espPart }

// Size reports the backing image size in bytes.
func (im *Image) Size() int64 { return im.size }

// Path reports the backing image path.
func (im *Image) Path() string { return im.path }

// BootPartition locates (without mounting) the Linux /boot partition, if any.
// Mounting it is a documented follow-up (see OpenImage); this lets callers
// inspect its geometry today.
func (im *Image) BootPartition() (gpt.Partition, bool, error) {
	f, err := osOpenFile(im.path, os.O_RDONLY, 0)
	if err != nil {
		return gpt.Partition{}, false, fmt.Errorf("grub: open image for /boot probe: %w", err)
	}
	defer f.Close()
	p, err := gptByType(f, im.size, gpt.LinuxFilesystemGUID)
	if err != nil {
		if errors.Is(err, gpt.ErrNotFound) {
			return gpt.Partition{}, false, nil
		}
		return gpt.Partition{}, false, fmt.Errorf("grub: locate /boot: %w", err)
	}
	return p, true, nil
}

// Close releases the mounted ESP filesystem, flushing any pending writes back
// to the disk image.
func (im *Image) Close() error {
	if im.esp != nil {
		return im.esp.Close()
	}
	return nil
}

// LocateGrubCfg returns the path of the grub.cfg on the given filesystem,
// trying the conventional locations and then any /EFI/<vendor>/grub.cfg.
// It returns ErrNoGrubCfg if none is present.
func LocateGrubCfg(fs filesystem.Filesystem) (string, error) {
	for _, p := range candidateCfgPaths {
		if _, err := fs.Stat(p); err == nil {
			return p, nil
		}
	}
	// Scan /EFI/<vendor>/grub.cfg.
	entries, err := fs.ListDir(efiRoot)
	if err == nil {
		// Deterministic order for reproducibility.
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		for _, name := range names {
			if name == "." || name == ".." {
				continue
			}
			cand := path.Join(efiRoot, name, "grub.cfg")
			if _, err := fs.Stat(cand); err == nil {
				return cand, nil
			}
		}
	}
	return "", ErrNoGrubCfg
}

// ReadGrubCfg locates and reads the grub.cfg on the ESP.
func (im *Image) ReadGrubCfg() (cfgPath string, content string, err error) {
	p, err := LocateGrubCfg(im.esp)
	if err != nil {
		return "", "", err
	}
	b, err := im.esp.ReadFile(p)
	if err != nil {
		return "", "", fmt.Errorf("grub: read %s: %w", p, err)
	}
	return p, string(b), nil
}

// WriteGrubCfg writes content to the given grub.cfg path on the ESP.
func (im *Image) WriteGrubCfg(cfgPath, content string) error {
	if err := im.esp.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("grub: write %s: %w", cfgPath, err)
	}
	return nil
}

// PatchQuiet removes the "quiet"/"splash" kernel flags and ensures serial
// console arguments on every linux line of the grub.cfg, editing the file
// through the mounted ESP filesystem (NOT raw disk bytes). It locates the
// grub.cfg, applies PatchGrubCfgContent, and writes it back. If the content is
// unchanged the file is left untouched.
//
// This replaces the old same-length raw-byte hack: the rewritten cmdline is
// shorter than the original (quiet/splash removed), which the FAT32 driver
// handles by re-truncating the file — something raw patching could never do
// without padding spaces into the kernel command line.
func (im *Image) PatchQuiet() (changed bool, err error) {
	cfgPath, content, err := im.ReadGrubCfg()
	if err != nil {
		return false, err
	}
	patched := PatchGrubCfgContent(content)
	if patched == content {
		return false, nil
	}
	if err := im.WriteGrubCfg(cfgPath, patched); err != nil {
		return false, err
	}
	return true, nil
}

// PatchQuietImage is the convenience wrapper matching the historical
// package-level entry point: open the image, FS-patch its grub.cfg, close.
// Unlike the removed raw-byte PatchQuiet, this performs real filesystem I/O
// and returns a meaningful error.
func PatchQuietImage(imagePath string) (bool, error) {
	im, err := OpenImage(imagePath)
	if err != nil {
		return false, err
	}
	defer im.Close()
	return im.PatchQuiet()
}
