module github.com/go-bootloaders/grub

go 1.26.4

require (
	github.com/go-filesystems/detect v0.0.0
	github.com/go-filesystems/detect/fat32reg v0.0.0-20260622100514-ad3c237ff7a8
	github.com/go-filesystems/fat32 v0.0.0
	github.com/go-filesystems/interface v0.0.0
	github.com/go-filesystems/uefi v0.0.0-20260622100157-d19717ea67ff
	github.com/go-tpm2/efitcg2 v0.2.0
	github.com/go-volumes/gpt v0.0.0-20260622100756-3721db1fbd05
)

require (
	github.com/go-tpm2/common v0.1.0 // indirect
	github.com/go-volumes/safeio v0.0.0-20260622072324-7f8eb19f6f8c // indirect
)

// interface is pinned at v0.0.0 by the driver modules (detect/fat32/uefi all
// carry `replace github.com/go-filesystems/interface => ../interface`, which
// does not compose for a downstream importer). It is satisfied here by a
// sibling checkout, exactly as the go-filesystems/detect/fat32reg adapter and
// the ext4/uefi consumers do. CI checks out go-filesystems/interface into a
// sibling `interface/` directory next to this repo.
replace github.com/go-filesystems/interface => ../interface

// detect and fat32 are required at v0.0.0 by the fat32reg adapter's go.mod
// (its own ../ replaces are local and do not propagate). Pin those v0.0.0
// requirements to the published pseudo-versions so the graph resolves from the
// public proxy without a sibling checkout of every driver.
replace github.com/go-filesystems/detect v0.0.0 => github.com/go-filesystems/detect v0.0.0-20260622100514-ad3c237ff7a8

replace github.com/go-filesystems/fat32 v0.0.0 => github.com/go-filesystems/fat32 v0.0.0-20260622082158-99c94157eb55
