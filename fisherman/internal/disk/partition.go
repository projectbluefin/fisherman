package disk

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/tuna-os/fisherman/internal/runner"
)

// procMountsPath is the path to the mounts file. Overridden in tests.
var procMountsPath = "/proc/mounts"

const GPTPartTypeLinuxRootX86_64 = "4f68bce3-e8cd-4db1-96e7-fbcaf984b709"

// GetProcMountsPath returns the current mounts file path. For testing only.
func GetProcMountsPath() string { return procMountsPath }

// SetProcMountsPath overrides the mounts file path. For testing only.
func SetProcMountsPath(p string) { procMountsPath = p }
func PartSuffix(disk string) string {
	if len(disk) == 0 {
		return ""
	}
	if unicode.IsDigit(rune(disk[len(disk)-1])) {
		return "p"
	}
	return ""
}

// PartName returns the full device path for partition num on disk.
// e.g. PartName("/dev/sda", 2) → "/dev/sda2"
//
//	PartName("/dev/nvme0n1", 2) → "/dev/nvme0n1p2"
func PartName(disk string, num int) string {
	return fmt.Sprintf("%s%s%d", disk, PartSuffix(disk), num)
}

// UnmountPartition releases kernel and userspace references to a specific partition
// in preparation for modifying its GPT attributes (e.g., retagging). This is less
// destructive than unmountAll() — it only affects the specific partition, not the
// entire disk. It does NOT deactivate LVM or kill processes.
func UnmountPartition(disk string, partNum int) error {
	partPath := PartName(disk, partNum)
	data, err := os.ReadFile(procMountsPath)
	if err != nil {
		return fmt.Errorf("reading /proc/mounts: %w", err)
	}

	// Find and unmount any mounts for this specific partition.
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		dev := fields[0]
		mp := fields[1]

		// Only process mounts for this specific partition.
		if dev != partPath {
			continue
		}

		// swap entries have mount point "none" and fstype "swap".
		if mp == "none" || (len(fields) >= 3 && fields[2] == "swap") {
			fmt.Fprintf(os.Stdout, "+ swapoff %s\n", dev)
			_ = runner.Run("swapoff", dev)
			continue
		}

		fmt.Fprintf(os.Stdout, "+ unmount %s (%s)\n", mp, dev)
		// Try udisksctl first — properly releases udisksd's open FD.
		if err := runner.Run("udisksctl", "unmount", "--no-user-interaction", "--block-device", dev); err != nil {
			// Fall back to umount -l for mounts not managed by udisks.
			_ = runner.Run("umount", "-l", mp)
		}
	}

	// Flush pending I/O so the kernel can drop its internal references.
	fmt.Fprintf(os.Stdout, "+ blockdev --flushbufs %s\n", partPath)
	_ = runner.Run("blockdev", "--flushbufs", partPath)

	// Give udev and udisksd time to release all device references.
	fmt.Fprintf(os.Stdout, "+ udevadm settle\n")
	_ = runner.Run("udevadm", "settle")

	return nil
}

// SetPartitionType rewrites the GPT type for a single partition on disk.
func SetPartitionType(disk string, partNum int, partType string) error {
	if err := runner.Run("sfdisk", "--part-type", disk, strconv.Itoa(partNum), partType); err != nil {
		return fmt.Errorf("setting partition type: %w", err)
	}
	return nil
}

// Partition wipes disk and creates a three-partition GPT layout using sfdisk:
//
//	Partition 1 – EFI System (512 MiB)
//	Partition 2 – /boot     (2 GiB, ext4 — bootloader reads this)
//	Partition 3 – Linux root (remaining space)
//
// A separate /boot partition is required because GRUB's built-in XFS driver
// does not support the newer XFS features enabled by mkfs.xfs on el10
// (nrext64, exchange, rmapbt). By keeping /boot on ext4, GRUB never needs
// to parse XFS. This matches what bootc install to-disk always does.
//
// 2 GiB provides headroom for the running deployment, a staged upgrade, and
// a rollback entry — each kernel+initramfs pair can be 200-400 MiB with large
// driver sets (e.g. NVIDIA), so 1 GiB fills up quickly once entries accumulate.
// The systemd-boot layout already uses 2 GiB for the same reason.
//
// On real block devices sfdisk notifies the kernel via BLKRRPART, so
// partition devices appear after udevadm settle. Loop devices reject
// BLKRRPART; we work around this by detaching and re-attaching the loop
// device with --partscan so the kernel creates /dev/loopNpM nodes.
func Partition(disk string) error {
	script := strings.Join([]string{
		"label: gpt",
		"",
		`size=512MiB, type=uefi, name="EFI-SYSTEM"`,
		`size=2GiB,   type=linux, name="boot"`,
		`type=linux, name="root"`,
	}, "\n") + "\n"
	return partition(disk, script)
}

// PartitionEncrypted wipes disk and creates the same three-partition GPT
// layout as Partition. The separate unencrypted /boot is also required for
// encrypted installs so that bootupctl (which runs in a restricted bwrap
// sandbox) can find the boot filesystem UUID from the raw block device.
func PartitionEncrypted(disk string) error {
	return Partition(disk)
}

// PartitionSystemdBoot wipes disk and creates a two-partition GPT layout for
// systemd-boot installs:
//
//	Partition 1 – EFI System (2 GiB — holds bootloader binary, kernel and initrd)
//	Partition 2 – Linux root (remaining space)
//
// Unlike grub2 installs, systemd-boot can only read FAT32 via UEFI firmware
// protocols. A separate ext4 /boot would be unreadable, so everything lands
// directly on the FAT32 ESP. Each kernel+initrd pair is ~200 MiB; 2 GiB
// comfortably accommodates the booted entry, rollback, and a staged upgrade
// without running out of space even if stale entries accumulate.
// This layout is used for both unencrypted and encrypted systemd-boot installs
// (encrypted: LUKS wraps partition 2).
func PartitionSystemdBoot(disk string) error {
	script := strings.Join([]string{
		"label: gpt",
		"",
		`size=2GiB, type=uefi, name="EFI-SYSTEM"`,
		`type=linux, name="root"`,
	}, "\n") + "\n"
	return partition(disk, script)
}

// PartitionZFS wipes disk and creates a two-partition GPT layout for
// ZFS installs:
//
//	Partition 1 – EFI System (1 GiB — holds bootloader, kernel, initrd)
//	Partition 2 – ZFS pool   (remaining space)
//
// ZFS installs use systemd-boot which reads directly from the FAT32 ESP.
// The ZFS pool on p2 holds the composefs-native bootc store.
func PartitionZFS(disk string) error {
	script := strings.Join([]string{
		"label: gpt",
		"",
		`size=1GiB, type=uefi, name="EFI-SYSTEM"`,
		`type=linux, name="zfs-pool"`,
	}, "\n") + "\n"
	return partition(disk, script)
}

// partition is the shared implementation for Partition and PartitionEncrypted.
func partition(disk, script string) error {
	// Unmount any mounted partitions on this disk before partitioning.
	if err := unmountAll(disk); err != nil {
		return fmt.Errorf("unmounting partitions: %w", err)
	}

	// First attempt without --force.
	err := runner.RunWithStdin(bytes.NewBufferString(script), "sfdisk", "--wipe=always", disk)
	if err != nil {
		// The disk may still be considered "in use" by the kernel even after
		// aggressive unmounting (e.g. a race with udev re-probing). Retry once
		// with --force --no-reread so sfdisk writes the table unconditionally.
		fmt.Fprintf(os.Stdout, "+ sfdisk first attempt failed (%v), retrying with --force\n", err)
		time.Sleep(500 * time.Millisecond)
		_ = runner.Run("udevadm", "settle")
		if err2 := runner.RunWithStdin(bytes.NewBufferString(script), "sfdisk", "--wipe=always", "--force", "--no-reread", disk); err2 != nil {
			return fmt.Errorf("sfdisk: %w", err2)
		}
		// --no-reread means the kernel still holds the OLD partition table.
		// Force a re-read with partprobe so subsequent mkfs calls see the
		// correct partition sizes. Without this, mkfs operates on stale
		// (possibly much smaller) partition boundaries → "no space left".
		fmt.Fprintf(os.Stdout, "+ partprobe %s (force kernel partition table re-read after --no-reread)\n", disk)
		time.Sleep(500 * time.Millisecond)
		_ = runner.Run("partprobe", disk)
		_ = runner.Run("udevadm", "settle")
	}

	// Brief sleep then settle — mirrors bootc's approach.
	time.Sleep(200 * time.Millisecond)
	_ = runner.Run("udevadm", "settle")

	// Loop devices reject the BLKRRPART ioctl that sfdisk uses to notify the
	// kernel.  Re-attach with --partscan so /dev/loopNpM nodes are created.
	if strings.HasPrefix(filepath.Base(disk), "loop") {
		if err := loopRescan(disk); err != nil {
			return fmt.Errorf("loop device rescan: %w", err)
		}
	}

	return nil
}

// FindSystemdBootPartitions returns the EFI and root partition paths after
// `bootc install to-disk` with systemd-boot. bootc creates a 3-partition GPT:
//
//	p1 = BIOS boot (1 MiB)
//	p2 = EFI System (512 MiB, FAT32)
//	p3 = Linux root (remainder, btrfs/xfs/ext4)
//
// It uses lsblk to confirm the layout rather than hardcoding partition numbers.
func FindSystemdBootPartitions(diskDev string) (efiPart, rootPart string, err error) {
	out, err := exec.Command("lsblk", "-J", "-p", "-o", "NAME,PARTTYPE", diskDev).Output()
	if err != nil {
		return "", "", fmt.Errorf("lsblk: %w", err)
	}
	type blockdev struct {
		Name     string     `json:"name"`
		Parttype string     `json:"parttype"`
		Children []blockdev `json:"children"`
	}
	type lsblkOutput struct {
		Blockdevices []blockdev `json:"blockdevices"`
	}
	var parsed lsblkOutput
	if err := json.Unmarshal(out, &parsed); err != nil {
		return "", "", fmt.Errorf("parsing lsblk output: %w", err)
	}
	const (
		efiGUID  = "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"
		biosGUID = "21686148-6449-6e6f-744e-656564454649"
	)
	for _, dev := range parsed.Blockdevices {
		for _, part := range dev.Children {
			switch strings.ToLower(part.Parttype) {
			case efiGUID:
				efiPart = part.Name
			case biosGUID:
				// skip
			default:
				if part.Name != "" {
					rootPart = part.Name
				}
			}
		}
	}
	if efiPart == "" || rootPart == "" {
		return "", "", fmt.Errorf("could not identify EFI and root partitions on %s", diskDev)
	}
	return efiPart, rootPart, nil
}

// RescanPartitions asks the kernel to re-read the partition table on disk.
// Used after `bootc install to-disk` which partitions the disk inside a
// container — the host kernel may not have seen the new partition layout yet.
// Returns nil even if the rescan is best-effort (e.g. non-loop block devices).
func RescanPartitions(d string) error {
	time.Sleep(200 * time.Millisecond)
	_ = runner.Run("udevadm", "settle")
	if strings.HasPrefix(filepath.Base(d), "loop") {
		return loopRescan(d)
	}
	// For real block devices, partprobe (or partx) triggers a kernel re-read.
	_ = runner.Run("partprobe", d)
	_ = runner.Run("udevadm", "settle")
	return nil
}

// loopRescan detaches a loop device and re-attaches it with --partscan (-P)
// so the kernel creates partition block devices (/dev/loopNpM).
func loopRescan(disk string) error {
	spawnArgs := func(name string, args ...string) (string, []string) {
		if inFlatpakEnv() {
			return "flatpak-spawn", append([]string{"--host", name}, args...)
		}
		return name, args
	}

	qname, qargs := spawnArgs("losetup", "--noheadings", "-O", "BACK-FILE", disk)
	out, err := exec.Command(qname, qargs...).Output()
	if err != nil {
		return fmt.Errorf("query backing file: %w", err)
	}
	backFile := strings.TrimSpace(string(out))

	dname, dargs := spawnArgs("losetup", "-d", disk)
	if err := exec.Command(dname, dargs...).Run(); err != nil {
		return fmt.Errorf("detach: %w", err)
	}
	rname, rargs := spawnArgs("losetup", "-P", disk, backFile)
	if err := exec.Command(rname, rargs...).Run(); err != nil {
		return fmt.Errorf("reattach with partscan: %w", err)
	}

	_ = runner.Run("udevadm", "settle")
	return nil
}

// unmountAll aggressively releases all kernel and userspace references to disk
// so that sfdisk can safely repartition it.
//
// Flash drives and USB disks are managed by udisksd, which holds an open FD
// on the block device even after umount -l. sfdisk detects that FD and refuses
// with "disk is currently in use". The sequence below is ordered from least to
// most destructive:
//
//  1. udisksctl unmount — releases udisksd's lock for automounted partitions.
//  2. umount -l         — lazy fallback for manually-mounted or non-udisks mounts.
//  3. swapoff           — disable any active swap partitions on the disk.
//  4. deactivateLVM     — deactivate LVM VGs and DM devices (dm-crypt etc.) that
//     hold the disk open; without this, sfdisk's BLKRRPART ioctl fails, forcing
//     the --no-reread path and leaving the kernel with a stale partition table.
//  5. fuser -km         — SIGKILL every process with an open FD on the disk.
//  6. blockdev --flushbufs — flush pending I/O so the kernel can drop references.
//  7. udevadm settle    — wait for udev/udisksd to release device references.
func unmountAll(disk string) error {
	data, err := os.ReadFile(procMountsPath)
	if err != nil {
		return fmt.Errorf("reading /proc/mounts: %w", err)
	}
	base := filepath.Base(disk) // e.g. "sda"
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		dev := fields[0] // e.g. /dev/sda1
		mp := fields[1]  // e.g. /run/media/james/Yellowfin_Live
		devBase := filepath.Base(dev)
		if !strings.HasPrefix(devBase, base) {
			continue
		}

		// swap entries have mount point "none" and fstype "swap".
		if mp == "none" || (len(fields) >= 3 && fields[2] == "swap") {
			fmt.Fprintf(os.Stdout, "+ swapoff %s\n", dev)
			_ = runner.Run("swapoff", dev)
			continue
		}

		fmt.Fprintf(os.Stdout, "+ unmount %s (%s)\n", mp, dev)

		// Try udisksctl first — properly releases udisksd's open FD.
		if err := runner.Run("udisksctl", "unmount", "--no-user-interaction", "--block-device", dev); err != nil {
			// Fall back to umount -l for mounts not managed by udisks.
			fmt.Fprintf(os.Stdout, "+ udisksctl failed (%v), falling back to umount -l\n", err)
			_ = runner.Run("umount", "-l", mp)
		}
	}

	// Deactivate LVM volume groups and device-mapper devices backed by this
	// disk. Without this, the LVM/dm layer holds the disk open and sfdisk's
	// BLKRRPART ioctl fails, forcing --no-reread and leaving the kernel with
	// a stale (potentially much smaller) partition table.
	deactivateLVM(disk)

	// Kill any processes still holding FDs open on any partition of this disk.
	// fuser exits non-zero when no processes are found — that is fine.
	fmt.Fprintf(os.Stdout, "+ fuser -km %s (kill remaining holders)\n", disk)
	_ = runner.Run("fuser", "-km", disk)

	// Flush pending I/O so the kernel can drop its internal references.
	_ = runner.Run("blockdev", "--flushbufs", disk)

	// Give udev and udisksd time to release all device references.
	_ = runner.Run("udevadm", "settle")
	return nil
}

// deactivateLVM removes LVM volume groups and device-mapper (dm-crypt, LUKS)
// devices that are backed by partitions of disk. This is required before
// repartitioning a disk that previously held LVM or LUKS — the device-mapper
// layer keeps an open reference on the underlying block device, causing sfdisk
// to fail with "disk is currently in use" even after all filesystems are
// unmounted.
func deactivateLVM(disk string) {
	base := filepath.Base(disk) // e.g. "sda"

	// Step 1: deactivate any LVM VGs that contain a PV on this disk.
	// `pvs` lists physical volumes; we match those whose PV name starts with
	// the disk path (e.g. /dev/sda1, /dev/sda2 ...).
	pvOut, err := exec.Command("pvs", "--noheadings", "-o", "pv_name,vg_name").Output()
	if err == nil {
		for _, line := range strings.Split(string(pvOut), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				continue
			}
			pvName, vgName := fields[0], fields[1]
			if strings.HasPrefix(pvName, disk) && vgName != "" {
				fmt.Fprintf(os.Stdout, "+ vgchange -an %s (LVM VG on %s)\n", vgName, pvName)
				_ = runner.Run("vgchange", "-an", vgName)
			}
		}
	}

	// Step 2: remove any device-mapper devices (LUKS, dm-crypt, LVM LVs)
	// whose block-device dependency includes a partition of this disk.
	// `dmsetup deps -o devname` prints the underlying devices for each mapper.
	dmList, err := exec.Command("dmsetup", "ls").Output()
	if err == nil {
		for _, line := range strings.Split(string(dmList), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			dmName := strings.Fields(line)[0]
			deps, err := exec.Command("dmsetup", "deps", "-o", "devname", dmName).Output()
			if err == nil && strings.Contains(string(deps), base) {
				fmt.Fprintf(os.Stdout, "+ dmsetup remove --force %s (backed by %s)\n", dmName, disk)
				_ = runner.Run("dmsetup", "remove", "--force", dmName)
			}
		}
	}
}

// inFlatpakEnv reports whether the current process is inside a Flatpak sandbox.
func inFlatpakEnv() bool {
	_, err := os.Stat("/.flatpak-info")
	return err == nil
}
