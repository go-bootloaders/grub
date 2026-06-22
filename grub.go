// Package grub is a pure-Go (CGO=0) GRUB administration toolkit for disk
// images. It is a production consumer of the go-volumes / go-filesystems /
// go-tpm2 stack:
//
//   - go-volumes/gpt locates the EFI System Partition and the Linux /boot (or
//     root) partition inside a GPT disk image.
//   - go-filesystems/detect probes each partition's filesystem type; grub then
//     mounts it writably through the matching concrete driver
//     (go-filesystems/fat32 for the ESP, go-filesystems/ext4 or
//     go-filesystems/btrfs for /boot) behind the common filesystem.Filesystem
//     interface.
//   - go-filesystems/uefi registers a GRUB UEFI boot entry (Boot####/BootOrder).
//   - go-tpm2/efitcg2 measures the grub.cfg + referenced kernel/initrd into the
//     TPM PCRs for measured boot (optional, behind an injected Caller).
//
// All grub.cfg editing is performed through the mounted filesystem's
// ReadFile/WriteFile — there is no raw same-length byte patching of the disk
// image. (RawReplaceAll survives only as a documented, deliberately-fenced
// low-level fallback; see raw.go.)
//
// Both the ESP and the /boot filesystem are opened in place via each driver's
// path+partition-index entry point (fat32.Open / ext4.Open / btrfs.Open), so
// WriteFile changes persist back to the partition's region of the disk image.
// The drivers expose a real WriteFile, so grub.cfg regeneration (MkConfig) and
// in-place patching (PatchQuiet) work against the /boot ext4/btrfs filesystem
// exactly as they do against the FAT32 ESP — no read-only temp-file staging.
package grub

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"

	btrfs "github.com/go-filesystems/btrfs"
	"github.com/go-filesystems/detect"
	ext4 "github.com/go-filesystems/ext4"
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

// ErrNoBoot is returned by the *OnBoot methods when no Linux /boot filesystem
// is mounted (the image has no Linux partition).
var ErrNoBoot = errors.New("grub: no /boot filesystem mounted")

// ErrUnsupportedBootFS is returned (wrapped) when the Linux partition exists
// but holds a filesystem grub cannot mount for /boot administration (i.e. not
// ext4 or btrfs).
var ErrUnsupportedBootFS = errors.New("grub: unsupported /boot filesystem type")

// Image is a mounted GRUB-administration handle over a GPT disk image. It owns
// the opened backing file, the mounted ESP filesystem, and — when present and
// of a supported type — the mounted Linux /boot filesystem; call Close to
// release them all. The /boot filesystem is mounted by OpenImage when a Linux
// partition is present and its filesystem (ext4 or btrfs) resolves cleanly.
type Image struct {
	path string
	size int64

	espPart gpt.Partition
	esp     filesystem.Filesystem

	bootPart gpt.Partition
	boot     filesystem.Filesystem
	bootType detect.Type
	hasBoot  bool
}

// Seams over os/io so each error branch is exercisable in tests.
var (
	osOpenFile = os.OpenFile
	gptByType  = gpt.ByType
	newSection = io.NewSectionReader
	detectType = detect.Detect
	fat32Open  = fat32.Open
	ext4Open   = ext4.Open
	btrfsOpen  = func(imagePath string, partIndex int) (filesystem.Filesystem, error) {
		// btrfs.Open returns the richer btrfs.FS interface; narrow it to the
		// common filesystem.Filesystem the mount layer works with.
		return btrfs.Open(imagePath, partIndex)
	}
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
// The Linux /boot partition (LinuxFilesystemGUID), when present, is mounted
// writably too: its filesystem type is probed via go-filesystems/detect and
// dispatched to the matching in-place driver (ext4.Open or btrfs.Open). A
// Linux partition holding an unsupported filesystem (neither ext4 nor btrfs)
// is reported as a hard error rather than silently skipped. An image with no
// Linux partition at all simply has no /boot mount (Boot() returns nil); this
// is the normal ESP-only case and not an error.
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

	im := &Image{
		path:    imagePath,
		size:    size,
		espPart: esp,
		esp:     fs,
	}

	// Mount the Linux /boot (or root) partition, if present, writably in place.
	if err := im.mountBoot(); err != nil {
		fs.Close()
		return nil, err
	}

	return im, nil
}

// mountBoot locates the Linux partition (LinuxFilesystemGUID), probes its
// filesystem type, and mounts ext4/btrfs filesystems writably in place via the
// matching driver (selecting the partition by its GPT index, so WriteFile
// changes persist to the disk image). It is a no-op when no Linux partition is
// present. An unsupported filesystem on a present Linux partition is an error.
func (im *Image) mountBoot() error {
	f, err := osOpenFile(im.path, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("grub: open image for /boot probe: %w", err)
	}
	part, err := gptByType(f, im.size, gpt.LinuxFilesystemGUID)
	if err != nil {
		f.Close()
		if errors.Is(err, gpt.ErrNotFound) {
			return nil // no Linux partition: ESP-only image, fine.
		}
		return fmt.Errorf("grub: locate /boot: %w", err)
	}

	typ, derr := detectType(newSection(f, part.StartOffset, part.Length), part.Length)
	if derr != nil {
		f.Close()
		return fmt.Errorf("grub: probe /boot type: %w", derr)
	}
	// Done probing through this handle; the writable mount re-opens the image.
	if cerr := f.Close(); cerr != nil {
		return fmt.Errorf("grub: close /boot probe handle: %w", cerr)
	}

	var bootFS filesystem.Filesystem
	switch typ {
	case detect.Ext4:
		bootFS, err = ext4Open(im.path, part.Index)
	case detect.Btrfs:
		bootFS, err = btrfsOpen(im.path, part.Index)
	default:
		return fmt.Errorf("%w: %s", ErrUnsupportedBootFS, typ)
	}
	if err != nil {
		return fmt.Errorf("grub: mount /boot (%s): %w", typ, err)
	}

	im.bootPart = part
	im.boot = bootFS
	im.bootType = typ
	im.hasBoot = true
	return nil
}

// ESP returns the mounted EFI System Partition filesystem.
func (im *Image) ESP() filesystem.Filesystem { return im.esp }

// ESPPartition returns the GPT geometry of the EFI System Partition.
func (im *Image) ESPPartition() gpt.Partition { return im.espPart }

// Size reports the backing image size in bytes.
func (im *Image) Size() int64 { return im.size }

// Path reports the backing image path.
func (im *Image) Path() string { return im.path }

// Boot returns the mounted Linux /boot filesystem, or nil when the image has
// no Linux partition. Use HasBoot to disambiguate a missing /boot from a nil
// check, and BootType to learn which driver (ext4/btrfs) backs it.
func (im *Image) Boot() filesystem.Filesystem { return im.boot }

// HasBoot reports whether a Linux /boot filesystem was mounted.
func (im *Image) HasBoot() bool { return im.hasBoot }

// BootType reports the detected filesystem type of the mounted /boot
// (detect.Ext4 or detect.Btrfs), or the empty Type when none is mounted.
func (im *Image) BootType() detect.Type { return im.bootType }

// BootPartition returns the GPT geometry of the Linux /boot partition and
// whether one is present. The partition is already mounted by OpenImage when
// of a supported type; this exposes its geometry for callers that need it.
func (im *Image) BootPartition() (gpt.Partition, bool, error) {
	if im.hasBoot {
		return im.bootPart, true, nil
	}
	// /boot was not mounted (absent). Re-probe to report geometry if a Linux
	// partition exists but holds an unmounted/unsupported FS — though in that
	// case OpenImage would have already errored. This keeps the accessor
	// honest for images opened by future code paths.
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

// Close releases the mounted ESP and /boot filesystems, flushing any pending
// writes back to the disk image. Both are closed even if the first errors; the
// first error encountered is returned.
func (im *Image) Close() error {
	var err error
	if im.boot != nil {
		if cerr := im.boot.Close(); cerr != nil {
			err = cerr
		}
	}
	if im.esp != nil {
		if cerr := im.esp.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
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

// ReadGrubCfgFS locates and reads the grub.cfg on an arbitrary mounted
// filesystem (the ESP or /boot). It is the shared core behind ReadGrubCfg and
// ReadGrubCfgOnBoot.
func ReadGrubCfgFS(fs filesystem.Filesystem) (cfgPath string, content string, err error) {
	p, err := LocateGrubCfg(fs)
	if err != nil {
		return "", "", err
	}
	b, err := fs.ReadFile(p)
	if err != nil {
		return "", "", fmt.Errorf("grub: read %s: %w", p, err)
	}
	return p, string(b), nil
}

// writeGrubCfgFS writes content to cfgPath on the given filesystem.
func writeGrubCfgFS(fs filesystem.Filesystem, cfgPath, content string) error {
	if err := fs.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("grub: write %s: %w", cfgPath, err)
	}
	return nil
}

// patchQuietFS locates the grub.cfg on fs, applies PatchGrubCfgContent, and
// writes it back when changed. It is the shared core behind PatchQuiet and
// PatchQuietOnBoot.
func patchQuietFS(fs filesystem.Filesystem) (changed bool, err error) {
	cfgPath, content, err := ReadGrubCfgFS(fs)
	if err != nil {
		return false, err
	}
	patched := PatchGrubCfgContent(content)
	if patched == content {
		return false, nil
	}
	if err := writeGrubCfgFS(fs, cfgPath, patched); err != nil {
		return false, err
	}
	return true, nil
}

// ReadGrubCfg locates and reads the grub.cfg on the ESP.
func (im *Image) ReadGrubCfg() (cfgPath string, content string, err error) {
	return ReadGrubCfgFS(im.esp)
}

// ReadGrubCfgOnBoot locates and reads the grub.cfg on the mounted /boot
// filesystem (where Debian/Fedora/etc. keep /boot/grub/grub.cfg or
// /boot/grub2/grub.cfg). It returns ErrNoBoot when no /boot is mounted.
func (im *Image) ReadGrubCfgOnBoot() (cfgPath string, content string, err error) {
	if !im.hasBoot {
		return "", "", ErrNoBoot
	}
	return ReadGrubCfgFS(im.boot)
}

// WriteGrubCfg writes content to the given grub.cfg path on the ESP.
func (im *Image) WriteGrubCfg(cfgPath, content string) error {
	return writeGrubCfgFS(im.esp, cfgPath, content)
}

// WriteGrubCfgOnBoot writes content to cfgPath on the mounted /boot filesystem.
// It returns ErrNoBoot when no /boot is mounted. The ext4/btrfs drivers expose
// a real WriteFile, so the change persists to the disk image.
func (im *Image) WriteGrubCfgOnBoot(cfgPath, content string) error {
	if !im.hasBoot {
		return ErrNoBoot
	}
	return writeGrubCfgFS(im.boot, cfgPath, content)
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
	return patchQuietFS(im.esp)
}

// PatchQuietOnBoot is PatchQuiet against the mounted /boot filesystem's
// grub.cfg (ext4/btrfs). It returns ErrNoBoot when no /boot is mounted. Like
// PatchQuiet, the rewritten (shorter) cmdline is truncated in place by the
// driver, and the change persists to the disk image.
func (im *Image) PatchQuietOnBoot() (changed bool, err error) {
	if !im.hasBoot {
		return false, ErrNoBoot
	}
	return patchQuietFS(im.boot)
}

// PatchQuietImage is the convenience wrapper matching the historical
// package-level entry point: open the image, FS-patch its grub.cfg, close. It
// patches the ESP grub.cfg, then the /boot grub.cfg when one is mounted, so a
// single call covers both the ESP-resident and the Debian-style /boot-resident
// configuration. It reports changed=true if either was modified. If neither
// the ESP nor /boot carries a grub.cfg at all it returns ErrNoGrubCfg (matching
// the historical single-target behaviour); a grub.cfg present on only one side
// is patched and the missing side is not an error.
func PatchQuietImage(imagePath string) (bool, error) {
	im, err := OpenImage(imagePath)
	if err != nil {
		return false, err
	}
	defer im.Close()

	changed := false
	foundCfg := false

	espChanged, espErr := im.PatchQuiet()
	switch {
	case espErr == nil:
		foundCfg = true
		changed = changed || espChanged
	case errors.Is(espErr, ErrNoGrubCfg):
		// No grub.cfg on the ESP; that's fine if /boot carries one.
	default:
		return false, espErr
	}

	if im.hasBoot {
		bootChanged, bootErr := im.PatchQuietOnBoot()
		switch {
		case bootErr == nil:
			foundCfg = true
			changed = changed || bootChanged
		case errors.Is(bootErr, ErrNoGrubCfg):
		default:
			return false, bootErr
		}
	}

	if !foundCfg {
		return false, ErrNoGrubCfg
	}
	return changed, nil
}
