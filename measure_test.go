package grub

import (
	"errors"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// fakeCaller is a test double for the efitcg2.Caller firmware seam. It records
// every measured event so the test can assert which PCRs were extended and how
// many measurements occurred. It implements efitcg2.Caller.
type fakeCaller struct {
	hashLogCalls int
	failAt       int // 1-based call index to fail at; 0 = never fail
	events       [][]byte
}

func (f *fakeCaller) SubmitCommand(inputBlock []byte, output []byte) (uintptr, error) {
	return 0, nil
}

func (f *fakeCaller) HashLogExtendEvent(flags uint64, dataToHash []byte, event []byte) (uintptr, error) {
	f.hashLogCalls++
	if f.failAt != 0 && f.hashLogCalls == f.failAt {
		return 0, errors.New("firmware measure failed")
	}
	cp := make([]byte, len(event))
	copy(cp, event)
	f.events = append(f.events, cp)
	return 0, nil
}

func TestMeasureBootEndToEnd(t *testing.T) {
	imgPath := buildESPImage(t, func(fs filesystem.Filesystem) {
		if err := fs.MkDir("/grub", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := fs.WriteFile("/grub/grub.cfg", []byte(sampleGrubCfg), 0o644); err != nil {
			t.Fatal(err)
		}
		seedKernels(t, fs)
	})
	im, err := OpenImage(imgPath)
	if err != nil {
		t.Fatalf("OpenImage: %v", err)
	}
	defer im.Close()

	fc := &fakeCaller{}
	m := NewMeasurer(fc)
	count, err := im.MeasureBoot(m)
	if err != nil {
		t.Fatalf("MeasureBoot: %v", err)
	}
	// 1 config + 2 kernels + 2 initrds = 5 measurements.
	if count != 5 {
		t.Fatalf("MeasureBoot count = %d, want 5", count)
	}
	if fc.hashLogCalls != 5 {
		t.Fatalf("HashLogExtendEvent calls = %d, want 5", fc.hashLogCalls)
	}
}

func TestMeasureIndividualPCRs(t *testing.T) {
	fc := &fakeCaller{}
	m := NewMeasurer(fc)
	if err := m.MeasureConfig([]byte("cfg")); err != nil {
		t.Fatalf("MeasureConfig: %v", err)
	}
	if err := m.MeasureFile("/boot/vmlinuz", []byte("k")); err != nil {
		t.Fatalf("MeasureFile: %v", err)
	}
	if err := m.MeasureLoadedImage("grubx64.efi", []byte("img")); err != nil {
		t.Fatalf("MeasureLoadedImage: %v", err)
	}
	if fc.hashLogCalls != 3 {
		t.Fatalf("calls = %d, want 3", fc.hashLogCalls)
	}
}

func TestMeasureConfigError(t *testing.T) {
	fc := &fakeCaller{failAt: 1}
	m := NewMeasurer(fc)
	if err := m.MeasureConfig([]byte("cfg")); err == nil {
		t.Fatal("expected MeasureConfig error")
	}
}

func TestMeasureFileError(t *testing.T) {
	fc := &fakeCaller{failAt: 1}
	m := NewMeasurer(fc)
	if err := m.MeasureFile("/x", []byte("d")); err == nil {
		t.Fatal("expected MeasureFile error")
	}
}

func TestMeasureLoadedImageError(t *testing.T) {
	fc := &fakeCaller{failAt: 1}
	m := NewMeasurer(fc)
	if err := m.MeasureLoadedImage("x", []byte("d")); err == nil {
		t.Fatal("expected MeasureLoadedImage error")
	}
}

func TestMeasureBootNoCfg(t *testing.T) {
	imgPath := buildESPImage(t, nil)
	im, err := OpenImage(imgPath)
	if err != nil {
		t.Fatalf("OpenImage: %v", err)
	}
	defer im.Close()
	if _, err := im.MeasureBoot(NewMeasurer(&fakeCaller{})); err != ErrNoGrubCfg {
		t.Fatalf("MeasureBoot err = %v, want ErrNoGrubCfg", err)
	}
}

func TestMeasureBootConfigFailure(t *testing.T) {
	imgPath := buildESPImage(t, func(fs filesystem.Filesystem) {
		if err := fs.MkDir("/grub", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := fs.WriteFile("/grub/grub.cfg", []byte(sampleGrubCfg), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	im, err := OpenImage(imgPath)
	if err != nil {
		t.Fatalf("OpenImage: %v", err)
	}
	defer im.Close()
	// Fail on the first measurement (the config).
	if _, err := im.MeasureBoot(NewMeasurer(&fakeCaller{failAt: 1})); err == nil {
		t.Fatal("expected config-measure failure to surface")
	}
}

func TestMeasureBootFileFailure(t *testing.T) {
	imgPath := buildESPImage(t, func(fs filesystem.Filesystem) {
		if err := fs.MkDir("/grub", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := fs.WriteFile("/grub/grub.cfg", []byte(sampleGrubCfg), 0o644); err != nil {
			t.Fatal(err)
		}
		seedKernels(t, fs)
	})
	im, err := OpenImage(imgPath)
	if err != nil {
		t.Fatalf("OpenImage: %v", err)
	}
	defer im.Close()
	// Fail on the 2nd measurement (first kernel), after config succeeds.
	if _, err := im.MeasureBoot(NewMeasurer(&fakeCaller{failAt: 2})); err == nil {
		t.Fatal("expected kernel-measure failure to surface")
	}
}
