package grub

import (
	"fmt"

	uefi "github.com/go-filesystems/uefi"
	"github.com/go-volumes/gpt"
)

// DefaultGrubLoaderPath is the conventional ESP path of the GRUB UEFI image.
const DefaultGrubLoaderPath = `\EFI\debian\grubx64.efi`

// LoadOptionAttributeActive marks the boot entry as active (LOAD_OPTION_ACTIVE).
const loadOptionActive = 0x00000001

// BuildBootEntry constructs the UEFI EFI_LOAD_OPTION for a GRUB loader living
// on the given GPT partition. The device path is
//
//	HD(<partno>,GPT,<part-guid>,<startLBA>,<sizeLBA>)/File(<loaderPath>)
//
// matching what firmware writes for a GRUB install. loaderPath is a
// backslash-separated EFI path such as DefaultGrubLoaderPath. partNumber is the
// 1-based GPT partition index; partGUID is the partition's unique GUID (raw
// 16 bytes, GPT mixed-endian as stored on disk).
func BuildBootEntry(description string, partNumber uint32, partGUID [16]byte, part gpt.Partition, loaderPath string) *uefi.LoadOption {
	const lba = 512
	hd := uefi.HardDriveNode{
		PartitionNumber: partNumber,
		PartitionStart:  uint64(part.StartOffset / lba),
		PartitionSize:   uint64(part.Length / lba),
		Signature:       partGUID,
		MBRType:         uefi.HDMBRTypeGPT,
		SignatureType:   uefi.HDSigTypeGUID,
	}
	return &uefi.LoadOption{
		Attributes:  loadOptionActive,
		Description: description,
		DevicePath: []uefi.DevicePathNode{
			hd.Node(),
			uefi.FilePathNode(loaderPath),
		},
	}
}

// RegisterBootEntry writes a GRUB boot entry into the given UEFI variable store
// and appends it to BootOrder, returning the assigned Boot#### number. The
// store is typically obtained via uefi.Open(<varstore.fd>); the grub package
// stays decoupled from how the store is materialised (an OVMF NvVar region, a
// firmware-backed store, etc.). It does not mount or modify the disk image.
func RegisterBootEntry(store uefi.VariableStore, lo *uefi.LoadOption) (uint16, error) {
	n, err := uefi.AddBootEntry(store, lo)
	if err != nil {
		return 0, fmt.Errorf("grub: register boot entry: %w", err)
	}
	return n, nil
}

// FindBootEntry returns the Boot#### number of an existing entry whose
// description matches description, or ok=false when none is present. It lets
// callers detect (and avoid duplicating) a previously-registered GRUB entry.
func FindBootEntry(store uefi.VariableStore, description string) (num uint16, ok bool, err error) {
	entries, _, err := uefi.ListBootEntries(store)
	if err != nil {
		return 0, false, fmt.Errorf("grub: list boot entries: %w", err)
	}
	for n, lo := range entries {
		if lo.Description == description {
			return n, true, nil
		}
	}
	return 0, false, nil
}
