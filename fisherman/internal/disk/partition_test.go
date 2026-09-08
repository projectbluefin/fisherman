package disk_test

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/tuna-os/fisherman/internal/disk"
	"github.com/tuna-os/fisherman/internal/runner"
)

// execCall and recorder are shared across all disk package tests in this directory.
type execCall struct {
	name  string
	args  []string
	stdin string
}

type recorder struct {
	calls []execCall
	err   error
}

func (r *recorder) run(stdin io.Reader, name string, args ...string) error {
	s := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		s = string(b)
	}
	r.calls = append(r.calls, execCall{name: name, args: args, stdin: s})
	return r.err
}

func setupRecorder(t *testing.T) *recorder {
	t.Helper()
	rec := &recorder{}
	runner.RunFn = rec.run
	t.Cleanup(func() { runner.RunFn = runner.DefaultRun })
	return rec
}

// ── PartSuffix / PartName ──────────────────────────────────────────────────

func TestPartSuffix(t *testing.T) {
	tests := []struct {
		disk string
		want string
	}{
		{"/dev/nvme0n1", "p"},
		{"/dev/mmcblk0", "p"},
		{"/dev/sda", ""},
		{"/dev/vda", ""},
		{"/dev/xvda", ""},
		{"", ""},
	}
	for _, tt := range tests {
		got := disk.PartSuffix(tt.disk)
		if got != tt.want {
			t.Errorf("PartSuffix(%q) = %q, want %q", tt.disk, got, tt.want)
		}
	}
}

func TestPartName(t *testing.T) {
	tests := []struct {
		disk string
		num  int
		want string
	}{
		{"/dev/nvme0n1", 1, "/dev/nvme0n1p1"},
		{"/dev/nvme0n1", 3, "/dev/nvme0n1p3"},
		{"/dev/sda", 1, "/dev/sda1"},
		{"/dev/sda", 3, "/dev/sda3"},
		{"/dev/vda", 2, "/dev/vda2"},
	}
	for _, tt := range tests {
		got := disk.PartName(tt.disk, tt.num)
		if got != tt.want {
			t.Errorf("PartName(%q, %d) = %q, want %q", tt.disk, tt.num, got, tt.want)
		}
	}
}

func TestSetPartitionType(t *testing.T) {
	rec := setupRecorder(t)

	if err := disk.SetPartitionType("/dev/vda", 2, disk.GPTPartTypeLinuxRootX86_64); err != nil {
		t.Fatalf("SetPartitionType: %v", err)
	}

	if len(rec.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(rec.calls))
	}
	call := rec.calls[0]
	if call.name != "sfdisk" {
		t.Fatalf("command = %q, want sfdisk", call.name)
	}
	wantArgs := []string{"--part-type", "/dev/vda", "2", disk.GPTPartTypeLinuxRootX86_64}
	if !equalSlice(call.args, wantArgs) {
		t.Errorf("args = %v, want %v", call.args, wantArgs)
	}
}

func TestUnmountPartition(t *testing.T) {
	rec := setupRecorder(t)

	// Mock /proc/mounts with some mounts for /dev/vda2 and other devices.
	mounts := `/dev/vda1 /boot ext4 rw,relatime 0 0
/dev/vda2 /mnt/target ext4 rw,relatime 0 0
/dev/vda3 /data btrfs rw,relatime 0 0
tmpfs /run tmpfs rw,nosuid 0 0
`
	tmpfile, err := os.CreateTemp("", "mounts")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer os.Remove(tmpfile.Name())
	if _, err := tmpfile.WriteString(mounts); err != nil {
		t.Fatalf("write mounts: %v", err)
	}
	tmpfile.Close()

	// Override procMountsPath for this test
	oldPath := disk.GetProcMountsPath()
	disk.SetProcMountsPath(tmpfile.Name())
	defer disk.SetProcMountsPath(oldPath)

	if err := disk.UnmountPartition("/dev/vda", 2); err != nil {
		t.Fatalf("UnmountPartition: %v", err)
	}

	// Should unmount only /dev/vda2, then flush and settle.
	if len(rec.calls) < 2 {
		t.Fatalf("expected at least 2 calls (udisksctl + blockdev), got %d", len(rec.calls))
	}

	// First call should be udisksctl unmount for /dev/vda2
	if rec.calls[0].name == "udisksctl" {
		if !equalSlice(rec.calls[0].args, []string{"unmount", "--no-user-interaction", "--block-device", "/dev/vda2"}) {
			t.Errorf("first call args = %v, want udisksctl unmount of /dev/vda2", rec.calls[0].args)
		}
	}

	// Should have blockdev --flushbufs call
	foundFlushbufs := false
	for _, call := range rec.calls {
		if call.name == "blockdev" && equalSlice(call.args, []string{"--flushbufs", "/dev/vda2"}) {
			foundFlushbufs = true
			break
		}
	}
	if !foundFlushbufs {
		t.Errorf("no blockdev --flushbufs call found for /dev/vda2")
	}

	// Should have udevadm settle call
	foundSettle := false
	for _, call := range rec.calls {
		if call.name == "udevadm" && equalSlice(call.args, []string{"settle"}) {
			foundSettle = true
			break
		}
	}
	if !foundSettle {
		t.Errorf("no udevadm settle call found")
	}
}

// ── Partition sfdisk script ────────────────────────────────────────────────

// sfdiskStdin finds the sfdisk call in rec.calls and returns its stdin content.
// Fails the test if sfdisk was not called exactly once.
func sfdiskStdin(t *testing.T, rec *recorder) string {
	t.Helper()
	var found []execCall
	for _, c := range rec.calls {
		if c.name == "sfdisk" {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly 1 sfdisk call, got %d (all calls: %+v)", len(found), rec.calls)
	}
	return found[0].stdin
}

// TestPartition_SfdiskScript verifies that Partition() produces a 3-partition
// GPT layout: EFI (2GiB) + /boot ext4 (2GiB) + root (remaining).
//
// This is a regression test for the 2→3 partition layout change. Any attempt
// to drop the separate ext4 /boot partition will be caught here.
func TestPartition_SfdiskScript(t *testing.T) {
	rec := setupRecorder(t)

	// /dev/sda: doesn't start with "loop", so loopRescan won't be called.
	if err := disk.Partition("/dev/sda"); err != nil {
		t.Fatalf("Partition: %v", err)
	}

	script := sfdiskStdin(t, rec)

	// Must declare a GPT partition table.
	if !strings.Contains(script, "label: gpt") {
		t.Errorf("sfdisk script missing 'label: gpt':\n%s", script)
	}

	// Count non-empty, non-label lines — these are the partition lines.
	var partLines []string
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "label:") {
			continue
		}
		partLines = append(partLines, line)
	}
	if len(partLines) != 3 {
		t.Errorf("expected exactly 3 partition lines, got %d:\n%s", len(partLines), script)
	}

	// Partition 1: EFI System, 2 GiB — use full token to distinguish from /boot.
	if !strings.Contains(partLines[0], "size=2GiB") || !strings.Contains(partLines[0], "type=uefi") {
		t.Errorf("EFI partition (size=2GiB, type=uefi) not found in sfdisk script:\n%s", script)
	}

	// Partition 2: Linux /boot, 2 GiB — check the second partition line directly.
	// Must have an explicit size (so it doesn't consume remaining space).
	if !strings.Contains(partLines[1], "size=2GiB") || !strings.Contains(partLines[1], "type=linux") {
		t.Errorf("/boot partition (size=2GiB, type=linux) not found in sfdisk script:\n%s", script)
	}

	// Partition 3: root — must NOT have a size= field (fills remaining space).
	rootLine := partLines[2]
	if strings.Contains(rootLine, "size=") {
		t.Errorf("root partition should have no size= (fills remaining space), got: %q", rootLine)
	}
	if !strings.Contains(rootLine, "type=linux") {
		t.Errorf("root partition missing type=linux: %q", rootLine)
	}
}

// TestPartitionEncrypted_SameLayout verifies that PartitionEncrypted produces
// the identical 3-partition sfdisk script as Partition. Both encrypted and
// unencrypted installs share the same disk layout; only LUKS setup differs.
func TestPartitionEncrypted_SameLayout(t *testing.T) {
	recPlain := setupRecorder(t)
	if err := disk.Partition("/dev/sda"); err != nil {
		t.Fatalf("Partition: %v", err)
	}
	plainScript := sfdiskStdin(t, recPlain)

	// Reset recorder for the encrypted call.
	recEnc := setupRecorder(t)
	if err := disk.PartitionEncrypted("/dev/sda"); err != nil {
		t.Fatalf("PartitionEncrypted: %v", err)
	}
	encScript := sfdiskStdin(t, recEnc)

	if plainScript != encScript {
		t.Errorf("PartitionEncrypted sfdisk script differs from Partition:\nplain:\n%s\nencrypted:\n%s", plainScript, encScript)
	}
}

// TestPartitionSystemdBoot_SfdiskScript verifies that PartitionSystemdBoot()
// produces a 2-partition GPT layout with a 2 GiB EFI partition.
// Each kernel+initrd pair is ~200 MiB; 2 GiB accommodates booted + rollback +
// a staged upgrade without exhausting the ESP.
func TestPartitionSystemdBoot_SfdiskScript(t *testing.T) {
	rec := setupRecorder(t)

	if err := disk.PartitionSystemdBoot("/dev/nvme0n1"); err != nil {
		t.Fatalf("PartitionSystemdBoot: %v", err)
	}

	script := sfdiskStdin(t, rec)

	if !strings.Contains(script, "label: gpt") {
		t.Errorf("sfdisk script missing 'label: gpt':\n%s", script)
	}

	// Count partition lines.
	var partLines []string
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "label:") {
			continue
		}
		partLines = append(partLines, line)
	}
	if len(partLines) != 2 {
		t.Errorf("expected exactly 2 partition lines, got %d:\n%s", len(partLines), script)
	}

	// Partition 1: EFI System, 2 GiB.
	if !strings.Contains(script, "size=2GiB") || !strings.Contains(script, "type=uefi") {
		t.Errorf("EFI partition (size=2GiB, type=uefi) not found in sfdisk script:\n%s", script)
	}

	// Partition 2: root — must NOT have a size= field (fills remaining space).
	rootLine := partLines[1]
	if strings.Contains(rootLine, "size=") {
		t.Errorf("root partition should have no size= (fills remaining space), got: %q", rootLine)
	}
	// Root must carry the Discoverable Partitions root GUID, not the generic
	// `linux` alias. With a Unified Kernel Image the cmdline is signed inside the
	// image, so EnsureLuksArgs cannot inject rd.luks.name= and there is no BLS
	// entry to edit; systemd-gpt-auto-generator is then the only way the root is
	// ever found. Typed `linux`, an encrypted install hangs ~90s on
	// dev-gpt-auto-root.device and lands in an emergency shell.
	if !strings.Contains(rootLine, "type="+disk.GPTPartTypeLinuxRootX86_64) {
		t.Errorf("root partition must be typed %s, got: %q",
			disk.GPTPartTypeLinuxRootX86_64, rootLine)
	}
	if strings.Contains(rootLine, "type=linux") {
		t.Errorf("root partition still uses the generic linux alias: %q", rootLine)
	}
}

// TestPartition_SfdiskArgs verifies the command-line arguments passed to sfdisk.
func TestPartition_SfdiskArgs(t *testing.T) {
	rec := setupRecorder(t)
	if err := disk.Partition("/dev/sda"); err != nil {
		t.Fatalf("Partition: %v", err)
	}

	var sfdiskCall *execCall
	for i := range rec.calls {
		if rec.calls[i].name == "sfdisk" {
			sfdiskCall = &rec.calls[i]
			break
		}
	}
	if sfdiskCall == nil {
		t.Fatal("sfdisk not called")
	}
	wantArgs := []string{"--wipe=always", "/dev/sda"}
	if !equalSlice(sfdiskCall.args, wantArgs) {
		t.Errorf("sfdisk args = %v, want %v", sfdiskCall.args, wantArgs)
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestUnmountAll_AutomountedDisk verifies that when a target disk has
// automounted partitions, udisksctl unmount, fuser, and blockdev are all
// called before sfdisk.
// This is the regression test for the flash-drive "disk is currently in use"
// error: umount -l releases the kernel mount but leaves udisksd's open FD;
// udisksctl unmount properly releases that FD.
func TestUnmountAll_AutomountedDisk(t *testing.T) {
	// Write a fake /proc/mounts with an automounted flash-drive partition.
	mounts := "/dev/sda1 /run/media/james/MYFLASH vfat rw 0 0\n" +
		"/dev/sdb1 /run/media/james/other vfat rw 0 0\n" // unrelated disk — must not be touched
	f, err := os.CreateTemp(t.TempDir(), "mounts")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(mounts); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Point the package at our fake mounts file.
	disk.SetProcMountsPath(f.Name())
	t.Cleanup(func() { disk.SetProcMountsPath("/proc/mounts") })

	rec := setupRecorder(t)

	if err := disk.Partition("/dev/sda"); err != nil {
		t.Fatalf("Partition: %v", err)
	}

	// All of these must appear before sfdisk.
	udisksIdx := -1
	fuserIdx := -1
	blockdevIdx := -1
	sfdiskIdx := -1
	for i, c := range rec.calls {
		switch c.name {
		case "udisksctl":
			if udisksIdx == -1 {
				udisksIdx = i
			}
		case "fuser":
			if fuserIdx == -1 {
				fuserIdx = i
			}
		case "blockdev":
			if blockdevIdx == -1 {
				blockdevIdx = i
			}
		case "sfdisk":
			if sfdiskIdx == -1 {
				sfdiskIdx = i
			}
		}
	}
	if udisksIdx == -1 {
		t.Error("udisksctl not called — automounted partition lock not released")
	}
	if fuserIdx == -1 {
		t.Error("fuser not called — open FD holders may not be killed")
	}
	if blockdevIdx == -1 {
		t.Error("blockdev --flushbufs not called")
	}
	if sfdiskIdx == -1 {
		t.Fatal("sfdisk not called")
	}
	if udisksIdx > sfdiskIdx {
		t.Errorf("udisksctl (idx %d) called after sfdisk (idx %d)", udisksIdx, sfdiskIdx)
	}
	if fuserIdx > sfdiskIdx {
		t.Errorf("fuser (idx %d) called after sfdisk (idx %d)", fuserIdx, sfdiskIdx)
	}
	if blockdevIdx > sfdiskIdx {
		t.Errorf("blockdev (idx %d) called after sfdisk (idx %d)", blockdevIdx, sfdiskIdx)
	}

	// udisksctl must target /dev/sda1, not /dev/sdb1.
	udisksCall := rec.calls[udisksIdx]
	wantArgs := []string{"unmount", "--no-user-interaction", "--block-device", "/dev/sda1"}
	if !equalSlice(udisksCall.args, wantArgs) {
		t.Errorf("udisksctl args = %v, want %v", udisksCall.args, wantArgs)
	}
}

// TestUnmountAll_NoMounts verifies that Partition succeeds cleanly when
// no partitions of the target disk are mounted (common case for NVMe install targets).
func TestUnmountAll_NoMounts(t *testing.T) {
	mounts := "/dev/sdb1 /run/media/james/other vfat rw 0 0\n"
	f, err := os.CreateTemp(t.TempDir(), "mounts")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(mounts)
	f.Close()

	disk.SetProcMountsPath(f.Name())
	t.Cleanup(func() { disk.SetProcMountsPath("/proc/mounts") })

	rec := setupRecorder(t)

	if err := disk.Partition("/dev/sda"); err != nil {
		t.Fatalf("Partition: %v", err)
	}

	for _, c := range rec.calls {
		if c.name == "udisksctl" || c.name == "umount" {
			t.Errorf("unexpected unmount call when disk has no mounts: %v %v", c.name, c.args)
		}
	}
}

// sfdiskFailOnceRecorder returns an error on the FIRST sfdisk call and nil
// for all subsequent calls (including the --force retry). This simulates the
// "disk is currently in use" error that triggers the --force --no-reread path.
type sfdiskFailOnceRecorder struct {
	calls       []execCall
	sfdiskCount int
}

func (r *sfdiskFailOnceRecorder) run(stdin io.Reader, name string, args ...string) error {
	s := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		s = string(b)
	}
	r.calls = append(r.calls, execCall{name: name, args: args, stdin: s})
	if name == "sfdisk" {
		r.sfdiskCount++
		if r.sfdiskCount == 1 {
			return fmt.Errorf("disk is currently in use")
		}
	}
	return nil
}

// TestPartition_PartprobeAfterForceReread is a regression test for the bug
// where a disk previously used for LVM caused sfdisk to fall back to
// --force --no-reread, leaving the kernel with the OLD (stale, potentially
// much smaller) partition table. The next mkfs then formatted the wrong-sized
// partition → "no space left on device" during bootc install.
//
// Fix: after --force --no-reread, partprobe must be called so the kernel
// re-reads the new partition table before any mkfs operations.
func TestPartition_PartprobeAfterForceReread(t *testing.T) {
	// Write an empty mounts file so unmountAll doesn't try real mounts.
	f, err := os.CreateTemp(t.TempDir(), "mounts")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	disk.SetProcMountsPath(f.Name())
	t.Cleanup(func() { disk.SetProcMountsPath("/proc/mounts") })

	rec := &sfdiskFailOnceRecorder{}
	runner.RunFn = rec.run
	t.Cleanup(func() { runner.RunFn = runner.DefaultRun })

	if err := disk.Partition("/dev/sda"); err != nil {
		t.Fatalf("Partition: %v", err)
	}

	// Find the indices of the second sfdisk call (--force --no-reread) and partprobe.
	secondSfdiskIdx := -1
	sfdiskCount := 0
	partprobeIdx := -1
	for i, c := range rec.calls {
		if c.name == "sfdisk" {
			sfdiskCount++
			if sfdiskCount == 2 {
				secondSfdiskIdx = i
			}
		}
		if c.name == "partprobe" && partprobeIdx == -1 {
			partprobeIdx = i
		}
	}

	if sfdiskCount != 2 {
		t.Fatalf("expected 2 sfdisk calls (first fails, second --force), got %d", sfdiskCount)
	}

	// Verify the retry used --force --no-reread.
	forceCall := rec.calls[secondSfdiskIdx]
	if !containsArg(forceCall.args, "--force") {
		t.Errorf("second sfdisk call missing --force: %v", forceCall.args)
	}
	if !containsArg(forceCall.args, "--no-reread") {
		t.Errorf("second sfdisk call missing --no-reread: %v", forceCall.args)
	}

	// Critical: partprobe must have been called after the --force sfdisk.
	if partprobeIdx == -1 {
		t.Fatal("partprobe not called after --force --no-reread sfdisk; " +
			"kernel will retain stale partition table causing mkfs to format wrong-sized partitions")
	}
	if partprobeIdx < secondSfdiskIdx {
		t.Errorf("partprobe (idx %d) called before --force sfdisk (idx %d); must be after",
			partprobeIdx, secondSfdiskIdx)
	}
}

func containsArg(args []string, target string) bool {
	for _, a := range args {
		if a == target {
			return true
		}
	}
	return false
}
