package install

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// storageDriverCandidate represents a storage driver choice and reasoning.
type storageDriverCandidate struct {
	driver string // "overlay" or "vfs"
	reason string // human-readable explanation
}

// selectStorageDriver chooses a storage driver for podman when --root is redirected
// to a scratch directory (needed for both composefs and non-composefs installs when
// the host's /var/lib/containers is space- or memory-constrained).
// Returns ("overlay", "reason") when safe, or ("vfs", "reason") as fallback.
// Checks include:
// - Filesystem type of scratchPath (rejects overlayfs, tmpfs)
// - Explicit podman overlay probe to verify the driver works on the target root
func selectStorageDriver(scratchPath string) (driver, reason string) {
	candidate := overlayCandidate(scratchPath)
	if candidate.driver == "vfs" {
		return candidate.driver, candidate.reason
	}

	// Probe podman overlay support on the target root.
	if err := probeOverlay(scratchPath); err != nil {
		return "vfs", fmt.Sprintf("podman overlay probe failed: %v", err)
	}

	return "overlay", "overlay-backed storage on safe filesystem"
}

// overlayCandidate checks if the scratch filesystem is safe for overlay, returning either
// ("overlay", "...") or ("vfs", "fallback reason").
func overlayCandidate(scratchPath string) storageDriverCandidate {
	fsType, err := filesystemType(scratchPath)
	if err != nil {
		return storageDriverCandidate{"vfs", fmt.Sprintf("could not detect filesystem type: %v", err)}
	}

	// Filesystems where overlay should NOT be used.
	unsafeFS := map[string]bool{
		"overlayfs": true,
		"tmpfs":     true,
	}

	if unsafeFS[fsType] {
		return storageDriverCandidate{"vfs", fmt.Sprintf("scratch filesystem %s does not support overlay", fsType)}
	}

	// Unknown filesystem: be conservative and use vfs.
	knownSafeFS := map[string]bool{
		"ext4":  true,
		"xfs":   true,
		"btrfs": true,
	}
	if !knownSafeFS[fsType] {
		return storageDriverCandidate{"vfs", fmt.Sprintf("scratch filesystem %s is not known to be overlay-safe", fsType)}
	}

	return storageDriverCandidate{"overlay", ""}
}

// filesystemType returns the filesystem type of the given path using statfs.
func filesystemType(path string) (string, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return "", err
	}

	// Map fstype magic numbers to names.
	// See include/uapi/linux/magic.h in the Linux kernel.
	fsTypes := map[int64]string{
		0x61756673: "aufs",
		0x9123683e: "btrfs",
		0x00c0ffee: "cramfs",
		0x00004d44: "msdos",
		0x3153464a: "jfs",
		0xf995e849: "hpfs",
		0x9660:     "isofs",
		0x137d:     "ext",
		0xef53:     "ext4", // ext2/ext3/ext4 share this magic; modern systems are ext4
		0xf2f52010: "ubifs",
		0x58465342: "xfs",
		0x794c7630: "overlayfs",
		0x01021994: "tmpfs",
		0x858458f6: "ramfs",
		0x6969:     "nfs",
	}

	if name, ok := fsTypes[st.Type]; ok {
		return name, nil
	}

	return fmt.Sprintf("unknown(0x%x)", st.Type), nil
}

// defaultStorageSpaceConstrained reports whether podman's default storage
// location lives on a memory-backed filesystem (tmpfs/ramfs/overlayfs) where
// a multi-gigabyte image pull would exhaust RAM. When the host has already
// provided disk-backed storage there (e.g. the wootc deployer bind-mounts an
// ext4 loop at /var/lib/containers), redirecting storage into the target disk
// is wasteful: it forces the OCI-export path, which lands three copies of the
// image inside the target (containers-root + oci-cache + the deployment) and
// overflows fixed-size targets.
func defaultStorageSpaceConstrained() bool {
	for _, p := range []string{"/var/lib/containers", "/var"} {
		if fsType, err := filesystemType(p); err == nil {
			return fsType == "tmpfs" || fsType == "ramfs" || fsType == "overlayfs"
		}
	}
	return false
}

// probeOverlay attempts to verify that podman can use the overlay driver on the target root.
// It runs a minimal `podman info --storage-driver overlay` probe and returns any error.
// If the probe fails, the caller should fall back to vfs.
func probeOverlay(scratchPath string) error {
	// Create a temporary containers-root for the probe.
	probeRoot := scratchPath + "/.overlay-probe"
	if err := os.MkdirAll(probeRoot, 0755); err != nil {
		return fmt.Errorf("creating probe root: %w", err)
	}
	defer os.RemoveAll(probeRoot)

	// Run `podman --root <probeRoot> --storage-driver overlay info` to check overlay support.
	// If overlay is not available or doesn't work on this root, podman will error.
	cmd := exec.Command("podman", "--root", probeRoot, "--storage-driver", "overlay", "info", "--format", "json")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("podman probe failed: %w (output: %s)", err, output)
	}

	return nil
}
