package grub

import (
	"errors"
	"io"
	"os"
	"testing"

	"github.com/go-filesystems/detect"
	filesystem "github.com/go-filesystems/interface"
	"github.com/go-volumes/gpt"
)

func swap[T any](p *T, v T) func() { old := *p; *p = v; return func() { *p = old } }

// errFS is a filesystem stub whose every mutating/reading op can be steered to
// fail, used to drive the error branches of the grub layer without a real
// image.
type errFS struct {
	statErr     error
	listErr     error
	readErr     error
	writeErr    error
	mkdirErr    error
	files       map[string][]byte
	dirs        map[string][]filesystem.DirEntry
	closeErr    error
	statPresent map[string]bool
}

func newErrFS() *errFS {
	return &errFS{files: map[string][]byte{}, dirs: map[string][]filesystem.DirEntry{}, statPresent: map[string]bool{}}
}

func (f *errFS) Close() error { return f.closeErr }
func (f *errFS) ReadFile(p string) ([]byte, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	if b, ok := f.files[p]; ok {
		return b, nil
	}
	return nil, os.ErrNotExist
}
func (f *errFS) ListDir(p string) ([]filesystem.DirEntry, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if d, ok := f.dirs[p]; ok {
		return d, nil
	}
	return nil, os.ErrNotExist
}
func (f *errFS) Stat(p string) (filesystem.Stat, error) {
	if f.statErr != nil {
		return nil, f.statErr
	}
	if f.statPresent[p] {
		return filesystem.NewStat(0o644, 0, 1), nil
	}
	if _, ok := f.files[p]; ok {
		return filesystem.NewStat(0o644, 0, 1), nil
	}
	return nil, os.ErrNotExist
}
func (f *errFS) WriteFile(p string, data []byte, _ os.FileMode) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.files[p] = data
	return nil
}
func (f *errFS) ReadLink(string) (string, error) { return "", nil }
func (f *errFS) MkDir(p string, _ os.FileMode) error {
	if f.mkdirErr != nil {
		return f.mkdirErr
	}
	f.statPresent[p] = true
	return nil
}
func (f *errFS) DeleteFile(string) error     { return nil }
func (f *errFS) DeleteDir(string) error      { return nil }
func (f *errFS) Rename(string, string) error { return nil }

// --- OpenImage seam branches ----------------------------------------------

func TestOpenImageGPTError(t *testing.T) {
	imgPath := buildESPImage(t, nil)
	defer swap(&gptByType, func(io.ReaderAt, int64, [16]byte) (gpt.Partition, error) {
		return gpt.Partition{}, errors.New("gpt boom")
	})()
	if _, err := OpenImage(imgPath); err == nil {
		t.Fatal("expected gpt error")
	}
}

func TestOpenImageProbeError(t *testing.T) {
	imgPath := buildESPImage(t, nil)
	defer swap(&detectType, func(io.ReaderAt, int64) (detect.Type, error) {
		return "", errors.New("probe boom")
	})()
	if _, err := OpenImage(imgPath); err == nil {
		t.Fatal("expected probe error")
	}
}

func TestOpenImageNotFAT32(t *testing.T) {
	imgPath := buildESPImage(t, nil)
	defer swap(&detectType, func(io.ReaderAt, int64) (detect.Type, error) {
		return detect.Type("ext4"), nil
	})()
	if _, err := OpenImage(imgPath); err == nil {
		t.Fatal("expected non-FAT32 rejection")
	}
}

func TestOpenImageMountError(t *testing.T) {
	imgPath := buildESPImage(t, nil)
	defer swap(&fat32Open, func(string, int) (filesystem.Filesystem, error) {
		return nil, errors.New("mount boom")
	})()
	if _, err := OpenImage(imgPath); err == nil {
		t.Fatal("expected mount error")
	}
}

// --- grub.cfg R/W error branches via injected FS --------------------------

// imageWith returns an *Image backed by the given errFS (no real disk).
func imageWith(fs filesystem.Filesystem) *Image {
	return &Image{path: "mem", esp: fs}
}

func TestReadGrubCfgReadError(t *testing.T) {
	fs := newErrFS()
	fs.files["/grub/grub.cfg"] = []byte("x")
	fs.readErr = errors.New("read boom")
	im := imageWith(fs)
	if _, _, err := im.ReadGrubCfg(); err == nil {
		t.Fatal("expected read error")
	}
}

func TestWriteGrubCfgError(t *testing.T) {
	fs := newErrFS()
	fs.writeErr = errors.New("write boom")
	im := imageWith(fs)
	if err := im.WriteGrubCfg("/grub/grub.cfg", "x"); err == nil {
		t.Fatal("expected write error")
	}
}

func TestPatchQuietWriteError(t *testing.T) {
	fs := newErrFS()
	fs.files["/grub/grub.cfg"] = []byte(sampleGrubCfg)
	fs.writeErr = errors.New("write boom")
	im := imageWith(fs)
	if _, err := im.PatchQuiet(); err == nil {
		t.Fatal("expected patch write error")
	}
}

func TestMkConfigWriteError(t *testing.T) {
	fs := newErrFS()
	fs.files["/boot/vmlinuz-1.0"] = []byte("k")
	fs.dirs["/boot"] = []filesystem.DirEntry{
		filesystem.NewDirEntry(1, "vmlinuz-1.0", 1),
	}
	fs.writeErr = errors.New("write boom")
	im := imageWith(fs)
	if _, _, err := im.MkConfig(MkConfigOptions{}); err == nil {
		t.Fatal("expected MkConfig write error")
	}
}

func TestMkConfigMkdirError(t *testing.T) {
	fs := newErrFS()
	fs.files["/boot/vmlinuz-1.0"] = []byte("k")
	fs.dirs["/boot"] = []filesystem.DirEntry{
		filesystem.NewDirEntry(1, "vmlinuz-1.0", 1),
	}
	fs.statErr = os.ErrNotExist // so LocateGrubCfg misses and mkDirAll runs
	fs.mkdirErr = errors.New("mkdir boom")
	im := imageWith(fs)
	if _, _, err := im.MkConfig(MkConfigOptions{}); err == nil {
		t.Fatal("expected MkConfig mkdir error")
	}
}

func TestMeasureBootKernelReadError(t *testing.T) {
	fs := newErrFS()
	fs.files["/grub/grub.cfg"] = []byte(sampleGrubCfg)
	fs.dirs["/boot"] = []filesystem.DirEntry{
		filesystem.NewDirEntry(1, "vmlinuz-1.0", 1),
	}
	// grub.cfg reads fine; the kernel read fails.
	rf := &readFailFS{errFS: fs, failPath: "/boot/vmlinuz-1.0"}
	im := imageWith(rf)
	if _, err := im.MeasureBoot(NewMeasurer(&fakeCaller{})); err == nil {
		t.Fatal("expected kernel read error")
	}
}

// readFailFS fails ReadFile only for a specific path.
type readFailFS struct {
	*errFS
	failPath string
}

func (r *readFailFS) ReadFile(p string) ([]byte, error) {
	if p == r.failPath {
		return nil, errors.New("kernel read boom")
	}
	return r.errFS.ReadFile(p)
}

func TestBootPartitionOpenError(t *testing.T) {
	im := &Image{path: "/no/such/img", size: 1024}
	if _, _, err := im.BootPartition(); err == nil {
		t.Fatal("expected open error")
	}
}

func TestCloseError(t *testing.T) {
	fs := newErrFS()
	fs.closeErr = errors.New("close boom")
	im := imageWith(fs)
	if err := im.Close(); err == nil {
		t.Fatal("expected close error")
	}
	// nil esp is a no-op.
	if err := (&Image{}).Close(); err != nil {
		t.Fatalf("nil-esp Close = %v", err)
	}
}
