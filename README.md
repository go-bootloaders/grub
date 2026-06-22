# grub

[![ci](https://github.com/go-bootloaders/grub/actions/workflows/ci.yml/badge.svg)](https://github.com/go-bootloaders/grub/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-bootloaders/grub.svg)](https://pkg.go.dev/github.com/go-bootloaders/grub)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD--3--Clause-blue.svg)](LICENSE)

Pure-Go (`CGO=0`) **GRUB administration toolkit** for disk images. It is a
production consumer of the storage / firmware / boot stack:

- **[go-volumes/gpt](https://github.com/go-volumes/gpt)** locates the EFI System
  Partition and the Linux `/boot` (or root) partition inside a GPT disk image.
- **[go-filesystems/detect](https://github.com/go-filesystems/detect)** probes
  each partition's filesystem type, then grub mounts it **read/write** behind
  the common `filesystem.Filesystem` API through the matching in-place driver —
  **[fat32](https://github.com/go-filesystems/fat32)** for the ESP and
  **[ext4](https://github.com/go-filesystems/ext4)** or
  **[btrfs](https://github.com/go-filesystems/btrfs)** for `/boot`. So
  `grub.cfg` is edited as a real file, not by raw byte surgery on the image, on
  both the ESP and a Debian-style `/boot`.
- **[go-filesystems/uefi](https://github.com/go-filesystems/uefi)** registers a
  GRUB UEFI boot entry (`Boot####` / `BootOrder`).
- **[go-tpm2/efitcg2](https://github.com/go-tpm2/efitcg2)** measures the
  `grub.cfg` and the kernels/initrds it references into the conventional TPM
  PCRs for measured boot — optional, behind an injected firmware `Caller`, so
  the tool stays TPM-free when no TPM is present.

## Module

```text
github.com/go-bootloaders/grub
```

## What it does

| Capability                     | ESP entry point                          | `/boot` (ext4/btrfs) entry point  |
| ------------------------------ | ---------------------------------------- | --------------------------------- |
| Open + mount ESP **and** /boot | `OpenImage(path) (*Image, error)`        | same call; `Boot()`, `BootType()` |
| Read / locate `grub.cfg`       | `(*Image).ReadGrubCfg`, `LocateGrubCfg`  | `(*Image).ReadGrubCfgOnBoot`      |
| De-`quiet` / add consoles      | `(*Image).PatchQuiet`, `PatchQuietImage` | `(*Image).PatchQuietOnBoot`       |
| Generate a `grub.cfg`          | `(*Image).MkConfig`, `GenerateConfig`    | `(*Image).MkConfigOnBoot`         |
| Discover kernels/initrds       | `DiscoverKernels(im.ESP())`              | `DiscoverKernels(im.Boot())`      |
| UEFI boot entry                | `BuildBootEntry`, `RegisterBootEntry`    | —                                 |
| Measured boot (TPM)            | `NewMeasurer`, `(*Image).MeasureBoot`    | `(*Image).MeasureBootOnBoot`      |
| BIOS boot-code install         | `InstallToSectors`, `InstallMBR`         | —                                 |

`PatchQuietImage` is a one-call convenience that patches **both** the ESP and a
mounted `/boot` `grub.cfg`. The `/boot` filesystem is mounted in place by the
ext4/btrfs driver, so `WriteFile` (and therefore `PatchQuietOnBoot` /
`MkConfigOnBoot`) persists back to the partition — full read **and** write, not
the read-only temp-file staging the `detect` adapter would give.

All `grub.cfg` editing is performed through the mounted filesystem's
`ReadFile` / `WriteFile`. There is **no** same-length raw-byte patching of the
disk image; the previous `quiet`→spaces hack is gone. (`RawReplaceAll` survives
only as a documented, deliberately-fenced low-level fallback for raw,
filesystem-less regions such as a string baked into `core.img`.)

## Usage

### Strip `quiet`/`splash`, add serial consoles

```go
changed, err := grub.PatchQuietImage("disk.img")
// removes quiet/splash and ensures console=tty0 console=hvc0 on every
// linux line of the ESP's grub.cfg, persisting through the FAT32 driver.
```

### Generate a fresh `grub.cfg` from on-disk kernels

```go
im, err := grub.OpenImage("disk.img")
defer im.Close()

cfgPath, n, err := im.MkConfig(grub.MkConfigOptions{
    Distributor: "Debian GNU/Linux",
    Cmdline:     "ro console=tty0 console=hvc0",
})
// scans /boot (and the ESP) for vmlinuz*/initrd*, emits proper
// set/insmod headers + one menuentry{linux,initrd} block per kernel,
// newest first, and writes it to the ESP.
```

### Register a UEFI boot entry

```go
store, _ := uefi.Open("OVMF_VARS.fd")
defer store.Close()

lo := grub.BuildBootEntry("GRUB", 1, partGUID, esp, grub.DefaultGrubLoaderPath)
num, err := grub.RegisterBootEntry(store, lo) // appends to BootOrder
```

### Measure boot into the TPM (optional)

```go
m := grub.NewMeasurer(firmwareCaller) // efitcg2.Caller; omit when no TPM
count, err := im.MeasureBoot(m)
// grub.cfg -> PCR 8, each kernel/initrd -> PCR 9 (GRUB TCG conventions).
```

## Architecture support

Validated on all six 64-bit Go targets: `amd64`, `arm64`, `riscv64`, `loong64`,
`ppc64le`, and big-endian `s390x` (the byte-order canary for the GPT / FAT32 /
UEFI decoders). Native runners run the full race + coverage suite; the four
non-native arches run the cross-compiled test binaries under QEMU.

## `/boot` mounting (ext4 / btrfs)

`OpenImage` mounts the Linux `/boot` (or root) partition **read/write** when one
is present, alongside the ESP. The partition's filesystem type is probed with
`go-filesystems/detect` and dispatched to the matching in-place driver
(`ext4.Open` or `btrfs.Open`); both expose a real `WriteFile`, so `grub.cfg`
discovery, patching, regeneration and TPM measurement all work against
`/boot/grub/grub.cfg` (and `/boot/grub2/grub.cfg`, `/grub/grub.cfg`) exactly as
they do against the ESP:

```go
im, _ := grub.OpenImage("disk.img")
defer im.Close()

if im.HasBoot() {                      // a Linux partition was mounted
    fmt.Println(im.BootType())         // detect.Ext4 or detect.Btrfs
    cfg, content, _ := im.ReadGrubCfgOnBoot()
    kernels, _ := grub.DiscoverKernels(im.Boot())
    im.PatchQuietOnBoot()              // persists to the ext4/btrfs partition
    im.MkConfigOnBoot(grub.MkConfigOptions{Distributor: "Debian"})
}
```

A Linux partition holding an unsupported filesystem (neither ext4 nor btrfs) is
reported as `ErrUnsupportedBootFS` rather than silently skipped; an image with
no Linux partition simply has no `/boot` mount (`HasBoot()` is `false`, the
`*OnBoot` methods return `ErrNoBoot`). All deps resolve cleanly from the public
Go proxy by pseudo-version — no `replace => ../sibling`, no vendoring.

## License

BSD-3-Clause — see [LICENSE](LICENSE). Copyright (c) 2026, the
go-bootloaders/grub authors.
