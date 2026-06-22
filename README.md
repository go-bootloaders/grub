# grub

[![ci](https://github.com/go-bootloaders/grub/actions/workflows/ci.yml/badge.svg)](https://github.com/go-bootloaders/grub/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-bootloaders/grub.svg)](https://pkg.go.dev/github.com/go-bootloaders/grub)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD--3--Clause-blue.svg)](LICENSE)

Pure-Go (`CGO=0`) **GRUB administration toolkit** for disk images. It is a
production consumer of the storage / firmware / boot stack:

- **[go-volumes/gpt](https://github.com/go-volumes/gpt)** locates the EFI System
  Partition (and, for inspection, the Linux `/boot` partition) inside a GPT
  disk image.
- **[go-filesystems/detect](https://github.com/go-filesystems/detect)** +
  **[go-filesystems/fat32](https://github.com/go-filesystems/fat32)** mount the
  ESP (FAT32) **read/write** behind the common `filesystem.Filesystem` API — so
  `grub.cfg` is edited as a real file, not by raw byte surgery on the image.
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

| Capability                | Entry point                              |
| ------------------------- | ---------------------------------------- |
| Open + mount ESP (R/W)    | `OpenImage(path) (*Image, error)`        |
| Read / locate `grub.cfg`  | `(*Image).ReadGrubCfg`, `LocateGrubCfg`  |
| De-`quiet` / add consoles | `(*Image).PatchQuiet`, `PatchQuietImage` |
| Generate a `grub.cfg`     | `(*Image).MkConfig`, `GenerateConfig`    |
| Discover kernels/initrds  | `DiscoverKernels`                        |
| UEFI boot entry           | `BuildBootEntry`, `RegisterBootEntry`    |
| Measured boot (TPM)       | `NewMeasurer`, `(*Image).MeasureBoot`    |
| BIOS boot-code install    | `InstallToSectors`, `InstallMBR`         |

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

## Known limitation: ext4 `/boot`

The ESP/FAT32 path is fully supported and resolves cleanly from the public Go
proxy. Mounting an ext4 Linux `/boot` partition is **not yet** wired in:
`go-filesystems/ext4` currently pulls in the `go-diskimages` `v0.0.0` + replace
tangle that does not resolve in an isolated downstream build. `BootPartition()`
locates the `/boot` partition's geometry today; mounting it is a documented
follow-up, gated on the org-wide pseudo-version migration. This is by design —
no faking, no vendor hacks.

## License

BSD-3-Clause — see [LICENSE](LICENSE). Copyright (c) 2026, the
go-bootloaders/grub authors.
