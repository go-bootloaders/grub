package grub

import (
	"fmt"

	"github.com/go-tpm2/efitcg2"
)

// Conventional TPM PCR indices for GRUB measured boot, per the GRUB TCG spec
// and the EFI boot measurement conventions:
//
//   - PCR 4: the loaded boot image (the GRUB EFI binary / kernel image).
//   - PCR 8: GRUB's configuration and typed commands (grub.cfg contents).
//   - PCR 9: files GRUB loads on behalf of the OS (kernel, initrd).
const (
	PCRLoadedImage = 4
	PCRGrubConfig  = 8
	PCRGrubFiles   = 9
)

// EV_IPL is the TCG event type GRUB uses when measuring its configuration and
// loaded files into PCR 8/9 (Initial Program Load).
const evIPL uint32 = 0x0000000D

// evEFIBootServicesApplication is the event type for a loaded EFI application
// measured into PCR 4.
const evEFIBootServicesApplication uint32 = 0x80000003

// Measurer performs GRUB measured-boot extensions through an injected
// efitcg2.Caller (the firmware TPM2 protocol). Constructing it without a real
// TPM is a no-op-free design: the tool stays entirely TPM-free unless a Caller
// is supplied, mirroring efitcg2's own injection seam.
type Measurer struct {
	tcg *efitcg2.TCG2
}

// NewMeasurer wraps an efitcg2.Caller (the firmware-call mechanism the loader
// implements; in tests a fake Caller) into a Measurer.
func NewMeasurer(caller efitcg2.Caller) *Measurer {
	return &Measurer{tcg: efitcg2.New(caller)}
}

// MeasureConfig extends PCR 8 with the grub.cfg contents (EV_IPL), exactly as
// GRUB measures its configuration before acting on it.
func (m *Measurer) MeasureConfig(cfg []byte) error {
	if err := m.tcg.MeasureToPCR(PCRGrubConfig, evIPL, cfg, []byte("grub.cfg")); err != nil {
		return fmt.Errorf("grub: measure config into PCR %d: %w", PCRGrubConfig, err)
	}
	return nil
}

// MeasureFile extends PCR 9 with a file GRUB loads (kernel or initrd),
// tagging the event with the file's path as its description.
func (m *Measurer) MeasureFile(path string, data []byte) error {
	if err := m.tcg.MeasureToPCR(PCRGrubFiles, evIPL, data, []byte(path)); err != nil {
		return fmt.Errorf("grub: measure file %q into PCR %d: %w", path, PCRGrubFiles, err)
	}
	return nil
}

// MeasureLoadedImage extends PCR 4 with a loaded boot image (the kernel or the
// GRUB EFI binary itself), using the EFI boot-services-application event type.
func (m *Measurer) MeasureLoadedImage(desc string, image []byte) error {
	if err := m.tcg.MeasureToPCR(PCRLoadedImage, evEFIBootServicesApplication, image, []byte(desc)); err != nil {
		return fmt.Errorf("grub: measure loaded image into PCR %d: %w", PCRLoadedImage, err)
	}
	return nil
}

// MeasureBoot measures a complete GRUB boot into the conventional PCRs from a
// mounted Image: the grub.cfg into PCR 8, and every discovered kernel/initrd
// into PCR 9. It is the convenience entry point that ties OpenImage to the TPM.
// Returns the number of measurements made.
func (im *Image) MeasureBoot(m *Measurer) (int, error) {
	_, content, err := im.ReadGrubCfg()
	if err != nil {
		return 0, err
	}
	count := 0
	if err := m.MeasureConfig([]byte(content)); err != nil {
		return count, err
	}
	count++

	kernels, err := DiscoverKernels(im.esp)
	if err != nil {
		return count, err
	}
	for _, k := range kernels {
		data, rerr := im.esp.ReadFile(k.KernelPath)
		if rerr != nil {
			return count, fmt.Errorf("grub: read kernel %s: %w", k.KernelPath, rerr)
		}
		if err := m.MeasureFile(k.KernelPath, data); err != nil {
			return count, err
		}
		count++
		if k.InitrdPath != "" {
			idata, rerr := im.esp.ReadFile(k.InitrdPath)
			if rerr != nil {
				return count, fmt.Errorf("grub: read initrd %s: %w", k.InitrdPath, rerr)
			}
			if err := m.MeasureFile(k.InitrdPath, idata); err != nil {
				return count, err
			}
			count++
		}
	}
	return count, nil
}
