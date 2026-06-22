package grub

import (
	"bytes"
	"fmt"
	"os"
)

const sectorSize = 512

// InstallToSectors writes `code` into `numSectors` sectors starting at
// `startSector` (0-based). This is the legitimate BIOS boot-code install path:
// GRUB's core.img / boot.img live in the post-MBR gap (the sectors between the
// MBR and the first partition) on a BIOS/GPT-hybrid layout, which is a raw
// region with no filesystem. It returns an error if `code` is larger than the
// provided sector range.
func InstallToSectors(imagePath string, code []byte, startSector, numSectors int64) error {
	if numSectors <= 0 {
		return fmt.Errorf("numSectors must be > 0")
	}
	total := numSectors * sectorSize
	if int64(len(code)) > total {
		return fmt.Errorf("code size %d exceeds sector range %d", len(code), total)
	}
	f, err := os.OpenFile(imagePath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	off := startSector * sectorSize
	if _, err := f.WriteAt(code, off); err != nil {
		return err
	}
	return nil
}

// InstallMBR writes a minimal MBR boot sector. `code` must be at most 446 bytes
// (the MBR boot-code area). The function ensures the boot signature 0x55AA is
// set at offsets 510-511. Like InstallToSectors this targets a raw,
// filesystem-free region (sector 0) and is a legitimate boot-code install.
func InstallMBR(imagePath string, code []byte) error {
	if len(code) > 446 {
		return fmt.Errorf("boot code too large for MBR: %d > 446", len(code))
	}
	f, err := os.OpenFile(imagePath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteAt(code, 0); err != nil {
		return err
	}
	if _, err := f.WriteAt([]byte{0x55, 0xAA}, 510); err != nil {
		return err
	}
	return nil
}

// RawReplaceAll scans f in overlapping 1 MiB chunks and replaces every
// occurrence of from with to (which MUST be the same length) in-place.
//
// DEPRECATED / LOW-LEVEL FALLBACK. This is NOT how grub.cfg should be edited:
// the production path is OpenImage -> PatchQuiet (filesystem-based ReadFile/
// WriteFile through the mounted ESP), which can shorten the kernel command
// line correctly instead of padding it with spaces. RawReplaceAll is retained
// only for the rare case of patching a raw, filesystem-less region (e.g. a
// string baked into core.img) where no driver can mount the bytes. Prefer the
// FS path for anything residing in a real filesystem.
func RawReplaceAll(f *os.File, from, to []byte) {
	if len(from) != len(to) {
		return
	}
	const chunkSize = 1 << 20 // 1 MiB
	overlap := len(from) - 1
	if overlap < 0 {
		overlap = 0
	}
	buf := make([]byte, chunkSize+overlap)
	var fileOff int64
	for {
		n, readErr := f.ReadAt(buf, fileOff)
		if n == 0 {
			return
		}
		chunk := buf[:n]
		idx := 0
		for {
			pos := bytes.Index(chunk[idx:], from)
			if pos < 0 {
				break
			}
			absPos := fileOff + int64(idx+pos)
			_, _ = f.WriteAt(to, absPos)
			idx += pos + len(from)
		}
		if readErr != nil {
			return
		}
		delta := int64(n - overlap)
		fileOff += delta
	}
}
