package recipe

import (
	"encoding/json"
	"fmt"
	"os"
)

// Recipe describes a fisherman installation.
type Recipe struct {
	Disk            string     `json:"disk"`            // block device, e.g. "/dev/sda" (auto-partition)
	Filesystem      string     `json:"filesystem"`      // "xfs", "ext4", "btrfs", or "zfs"
	BtrfsSubvolumes bool       `json:"btrfsSubvolumes"` // create @, @home, @snapshots
	Encryption      Encryption `json:"encryption"`
	Image           string     `json:"image"`        // source OCI image reference
	TargetImgref    string     `json:"targetImgref"` // update-tracking ref (optional)
	SelinuxDisabled bool       `json:"selinuxDisabled"`
	UnifiedStorage  bool       `json:"unifiedStorage"` // pass --experimental-unified-storage
	// ComposeFsBackend passes --composefs-backend to bootc install to-filesystem.
	// Required for composefs-native images (e.g. ghcr.io/bootcrew/*).
	// Independent of UnifiedStorage — these are different bootc features.
	// Works with any supported filesystem including xfs.
	// Automatically forced to true when Filesystem is "zfs".
	ComposeFsBackend bool `json:"composeFsBackend"`
	// ZFSPoolName is the name of the ZFS pool to create (default: "rpool").
	// Only used when Filesystem is "zfs".
	ZFSPoolName string `json:"zfsPoolName,omitempty"`
	// Bootloader selects the bootloader: "grub2" (default) or "systemd".
	// Use "systemd" for images that ship systemd-boot (e.g. Project Bluefin/Dakota).
	Bootloader string `json:"bootloader"`
	// ImageType selects the install backend: "bootc" (default) or "ostree".
	// "ostree" support is not yet implemented and will be rejected by Validate().
	ImageType string `json:"imageType"`
	// FlatpakVarPath is the path relative to the install target root where the
	// writable /var for flatpaks lives. Defaults to "" which means fisherman
	// auto-detects based on whether the system is ostree or composefs-native.
	// Override in images.json for non-standard layouts, e.g. GnomeOS/Dakota:
	//   "flatpak_var_path": "state/os/default/var"
	FlatpakVarPath string   `json:"flatpakVarPath,omitempty"`
	Hostname       string   `json:"hostname"`
	Flatpaks       []string `json:"flatpaks"` // flatpak app IDs to install; empty = fallback
	User           UserSpec `json:"user"`     // optional user account to create
	// CustomMounts is set for manual partitioning. When non-empty, Disk/Filesystem/
	// BtrfsSubvolumes and the auto-partition steps are skipped; fisherman formats and
	// mounts the listed partitions directly.
	CustomMounts []CustomMount `json:"customMounts,omitempty"`
	// VarDisk optionally describes a separate disk to mount at /var.
	// When set, fisherman formats (or mounts as-is) this disk before running
	// bootc, then adds a /var entry to the installed system's fstab.
	VarDisk *VarDiskSpec `json:"varDisk,omitempty"`
	// AdditionalImageStores lists host paths to be exposed to the bootc
	// container as containers/storage additionalimagestores. Each path is
	// bind-mounted read-only into the container at the same location and added
	// to a fisherman-generated storage.conf passed as CONTAINERS_STORAGE_CONF.
	//
	// Use this for live-media offline image stores (e.g. a squashfs of an OCI
	// store baked into an installer ISO). When the caller provides their own
	// CONTAINERS_STORAGE_CONF env var, that takes priority and this field is
	// ignored.
	AdditionalImageStores []string `json:"additionalImageStores,omitempty"`
	// SlurpWallpapers enables pre-partition extraction of wallpapers from an
	// existing Windows (NTFS) partition on the target disk. The wallpapers are
	// held in RAM (/run) and injected into the installed user's home directory
	// after the OS install completes. Entirely non-fatal — if no NTFS partition
	// is found or extraction fails, the install continues normally.
	SlurpWallpapers bool `json:"slurpWallpapers,omitempty"`
	// Slurp configures full user-data migration from an existing Windows
	// partition. When set, fisherman extracts the specified categories before
	// partitioning and injects them post-install. Takes priority over
	// SlurpWallpapers (which only grabs wallpapers).
	Slurp *SlurpSpec `json:"slurp,omitempty"`
	// TargetMount overrides the host path where fisherman assembles the
	// target filesystem hierarchy. Defaults to "/mnt/fisherman-target".
	// Use this to run multiple installs in parallel on the same host
	// (e.g. CI matrix on a single runner) without colliding mount points.
	TargetMount string `json:"targetMount,omitempty"`
	// LuksMapperName overrides the cryptsetup mapper name used for the
	// LUKS root device. Defaults to "fisherman-root". Like TargetMount,
	// this is intended primarily for parallel test runs — production
	// installs should leave it at the default since the same name is
	// hard-coded into installed system kernel cmdlines (rd.luks.name).
	LuksMapperName string `json:"luksMapperName,omitempty"`
	// DistroID is a short lowercase identifier for the distribution, used
	// to name OEM setup paths and service files written to the target
	// (e.g. /etc/<distroID>/oem/, <distroID>-oem-setup.service).
	// Defaults to "bootc" when empty.
	DistroID string `json:"distroID,omitempty"`
	// BrewTap is an optional Homebrew tap to add before installing OEM
	// packages (e.g. "ublue-os/tap"). When empty, no tap is added.
	BrewTap string `json:"brewTap,omitempty"`
}

// UserSpec describes a user account to create during installation.
// If Username is empty the user creation step is skipped.
type UserSpec struct {
	Username string   `json:"username"`
	Fullname string   `json:"fullname"`
	Password string   `json:"password"`
	Groups   []string `json:"groups"`
}

// SlurpSpec configures Windows user-data migration.
type SlurpSpec struct {
	SourcePartition string          `json:"sourcePartition"`
	Users           []SlurpUserSpec `json:"users"`
}

// SlurpUserSpec describes which categories to extract for one Windows user.
type SlurpUserSpec struct {
	Name       string   `json:"name"`
	Categories []string `json:"categories"`
}

// VarDiskSpec describes an optional separate disk to mount at /var.
type VarDiskSpec struct {
	Disk         string `json:"disk"`         // block device, e.g. "/dev/sdb"
	KeepExisting bool   `json:"keepExisting"` // if true, mount as-is; if false, format XFS
}

// isSupportedMountFstype reports whether a customMount fstype is one
// disk.formatPartition() can actually act on. Keep in sync with that switch:
// the empty string and "unformatted" mean "mount, do not format", which is what
// a pre-populated partition such as an existing ESP requires.
func isSupportedMountFstype(fstype string) bool {
	switch fstype {
	case "", "unformatted", "swap", "fat32", "ext3", "ext4", "xfs", "btrfs":
		return true
	default:
		return false
	}
}

// CustomMount describes a single partition → mountpoint mapping for manual layouts.
type CustomMount struct {
	Partition string `json:"partition"` // e.g. "/dev/sda1"
	Target    string `json:"target"`    // mountpoint, e.g. "/" or "/boot/efi"
	Fstype    string `json:"fstype"`    // e.g. "xfs", "fat32", "ext4", "unformatted"
}

// Encryption describes the disk encryption configuration.
type Encryption struct {
	Type       string `json:"type"`       // "none", "luks-passphrase", "tpm2-luks", "tpm2-luks-passphrase"
	Passphrase string `json:"passphrase"` // required for luks-passphrase and tpm2-luks-passphrase
}

// Load reads and parses a recipe JSON file.
func Load(path string) (*Recipe, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading recipe: %w", err)
	}
	var r Recipe
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parsing recipe: %w", err)
	}
	return &r, nil
}

// Validate checks that the recipe fields are coherent and that the disk exists.
func (r *Recipe) Validate() error {
	if len(r.CustomMounts) > 0 {
		// Manual layout: validate each mount spec instead of the auto-partition fields.
		hasRoot := false
		for i, cm := range r.CustomMounts {
			if cm.Partition == "" {
				return fmt.Errorf("customMounts[%d]: partition is required", i)
			}
			if cm.Target != "swap" && cm.Target != "" {
				if _, err := os.Stat(cm.Partition); err != nil {
					return fmt.Errorf("customMounts[%d]: partition %s: %w", i, cm.Partition, err)
				}
			}
			if cm.Target == "/" {
				hasRoot = true
			}
			// Validate the fstype here, where it is cheap and non-destructive.
			// disk.ApplyCustomLayout() only discovers an unsupported value once
			// it reaches formatPartition() — by which point the caller may
			// already have repartitioned a disk on the strength of this recipe
			// validating. A caller passing "vfat" (the obvious spelling, and
			// not one we accept) got exactly that: validation passed, the
			// install died mid-flight.
			if !isSupportedMountFstype(cm.Fstype) {
				return fmt.Errorf("customMounts[%d]: unsupported fstype %q "+
					"(supported: fat32, ext3, ext4, xfs, btrfs, swap, or "+
					"\"unformatted\"/\"\" to mount without formatting)", i, cm.Fstype)
			}
		}
		if !hasRoot {
			return fmt.Errorf("customMounts: no root (/) partition specified")
		}
		// Encryption is NOT applied on the manual path: luksFormat/luksOpen run
		// only in the auto-partition branch below, and TPM enrolment needs an
		// activeRootPart that manual mode leaves empty. Accepting an encrypted
		// manual recipe therefore produces an install that completes
		// UNENCRYPTED while the caller believes otherwise — a security-boundary
		// failure, so fail closed here rather than silently downgrade.
		// (Same shape as the ZFS+LUKS rejection below.)
		if r.Encryption.Type != "" && r.Encryption.Type != "none" {
			return fmt.Errorf("encryption %q is not supported with customMounts: "+
				"manual layouts do not run luksFormat, so the install would complete "+
				"unencrypted", r.Encryption.Type)
		}
	} else {
		if r.Disk == "" {
			return fmt.Errorf("disk is required")
		}
		if _, err := os.Stat(r.Disk); err != nil {
			return fmt.Errorf("disk %s: %w", r.Disk, err)
		}
		switch r.Filesystem {
		case "xfs", "ext4", "btrfs", "zfs":
		default:
			return fmt.Errorf("filesystem must be \"xfs\", \"ext4\", \"btrfs\", or \"zfs\", got %q", r.Filesystem)
		}
		if r.BtrfsSubvolumes && r.Filesystem != "btrfs" {
			return fmt.Errorf("btrfsSubvolumes requires filesystem=btrfs")
		}
		if r.Filesystem == "zfs" && r.Encryption.Type != "" && r.Encryption.Type != "none" {
			return fmt.Errorf("ZFS filesystem does not support LUKS encryption in this version")
		}
	}
	switch r.ImageType {
	case "", "bootc":
		// ok
	case "ostree":
		return fmt.Errorf("imageType \"ostree\" is not yet supported; only \"bootc\" is implemented")
	default:
		return fmt.Errorf("imageType must be \"bootc\" (or empty), got %q", r.ImageType)
	}
	switch r.Bootloader {
	case "", "grub2", "systemd":
		// ok
	default:
		return fmt.Errorf("bootloader must be \"grub2\" or \"systemd\", got %q", r.Bootloader)
	}
	switch r.Encryption.Type {
	case "", "none", "tpm2-luks", "luks-passphrase", "tpm2-luks-passphrase":
	default:
		return fmt.Errorf("encryption.type must be \"none\", \"luks-passphrase\", \"tpm2-luks\", or \"tpm2-luks-passphrase\"")
	}
	if (r.Encryption.Type == "luks-passphrase" || r.Encryption.Type == "tpm2-luks-passphrase") && r.Encryption.Passphrase == "" {
		return fmt.Errorf("encryption.passphrase required for %s", r.Encryption.Type)
	}
	// image may be empty in live-ISO mode; bootc auto-detects the running container.
	if r.VarDisk != nil {
		if r.VarDisk.Disk == "" {
			return fmt.Errorf("varDisk.disk is required")
		}
		if _, err := os.Stat(r.VarDisk.Disk); err != nil {
			return fmt.Errorf("varDisk.disk %s: %w", r.VarDisk.Disk, err)
		}
		if r.VarDisk.Disk == r.Disk {
			return fmt.Errorf("varDisk.disk must differ from the system disk")
		}
	}
	if r.Hostname == "" {
		return fmt.Errorf("hostname is required")
	}
	return nil
}
