package install_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/fisherman/internal/install"
)

// TestComposeFsMountStrategy_HostVarTmpBound is a regression test for #20/#21.
//
// Even with scratch bind-mounted at /var/tmp (and /tmp) in the bootc container,
// the install preflight hijacks the container's /var/tmp: bootc compares the
// statvfs f_fsid of the container /var/tmp against /proc/1/root/var/tmp (the
// HOST /var/tmp, visible because fisherman runs podman with --pid=host) and,
// on a live ISO those differ, so it replaces the disk-backed scratch bind with a
// recursive bind of the host's tiny dracut overlay. Meanwhile containers/image
// hardcodes /var/tmp for its multi-GiB big-file staging, so a storage.conf
// tmpdir cannot redirect it (the old f384208f approach): the timestamped env
// verification on PR #21 showed blobs still landing at
// /var/tmp/container_images_storage* and ENOSPCing.
//
// The fix engages HostVarTmpBindFn — a whole-install bind of the disk-backed
// scratch's var-tmp-override subdir over the HOST /var/tmp in the host mount
// namespace — so whichever /var/tmp bootc ends up using is disk-backed. The
// container-side scratch:/var/tmp, /tmp and TMPDIR mounts remain as belt-and-
// braces for processes outside bootc's mirroring path.
func TestComposeFsMountStrategy_HostVarTmpBound(t *testing.T) {
	tmpDir := t.TempDir()

	install.SkopeoExportOCIFn = func(image, destDir, tmpdir string) error {
		return os.MkdirAll(destDir, 0755)
	}
	t.Cleanup(func() { install.SkopeoExportOCIFn = install.DefaultSkopeoExportOCI })

	scratchDir := filepath.Join(tmpDir, "scratch")
	if err := os.MkdirAll(scratchDir, 0755); err != nil {
		t.Fatalf("mkdir scratch: %v", err)
	}
	target := filepath.Join(tmpDir, "target")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}

	gotScratch := stubHostVarTmpBind(t)

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w

	_ = install.BootcInstall(install.Options{
		ComposeFsBackend: true,
		SourceImgref:     "containers-storage:ghcr.io/projectbluefin/dakota:latest",
		TargetImgref:     "ghcr.io/projectbluefin/dakota:latest",
		Target:           target,
		ScratchDir:       scratchDir,
		NeedsPull:        false,
	})

	w.Close()
	os.Stdout = oldStdout
	var buf bytes.Buffer
	io.Copy(&buf, r) //nolint:errcheck
	output := buf.String()

	// The host /var/tmp binder must be engaged for the composefs container path,
	// using the same scratch dir that is bind-mounted into the container.
	if *gotScratch != scratchDir {
		t.Errorf("HostVarTmpBindFn called with %q, want %q", *gotScratch, scratchDir)
	}
	// The container still gets disk-backed /var/tmp, /tmp and TMPDIR.
	scratchMount := scratchDir + ":/var/tmp"
	if !strings.Contains(output, scratchMount) {
		t.Errorf("podman command missing disk-backed scratch mount %q\ngot: %s", scratchMount, output)
	}
	tmpMount := scratchDir + ":/tmp"
	if !strings.Contains(output, tmpMount) {
		t.Errorf("podman command missing disk-backed scratch mount %q\ngot: %s", tmpMount, output)
	}
	if !strings.Contains(output, "-e TMPDIR=/var/tmp") {
		t.Errorf("podman command missing TMPDIR=/var/tmp env var\ngot: %s", output)
	}
}

// TestComposeFsMountStrategy_Issue38 is a regression test for issue #38,
// updated for issue #20.
//
// Issue #38: When installing composefs images to btrfs targets with overlay
// storage driver, the entire scratch directory was mounted to /var/tmp. On
// btrfs-on-LUKS targets this caused the OCI cache to be invisible inside the
// bootc container:
//
//	"failed to invoke method OpenImage: open /var/tmp/oci-cache/index.json: no such file"
//
// The fix mounts the OCI cache at containerOCICachePath (/run/fisherman/oci-cache)
// — a dedicated path under /run that avoids /var/tmp interactions.
//
// Issue #20: even with the OCI cache at a dedicated mount, bootc's internal
// containers/storage writes layer blobs to its (hardcoded) temp dir /var/tmp
// (e.g. /var/tmp/container_images_storage*/…). With /var/tmp on a tmpfs that
// fills up → ENOSPC on multi-GiB composefs images. Because the OCI cache is no
// longer nested inside /var/tmp, the scratch dir can now be bind-mounted at
// /var/tmp again, giving bootc disk-backed scratch for its blob staging.
func TestComposeFsMountStrategy_Issue38(t *testing.T) {
	tmpDir := t.TempDir()

	install.SkopeoExportOCIFn = func(image, destDir, tmpdir string) error {
		return os.MkdirAll(destDir, 0755)
	}
	t.Cleanup(func() { install.SkopeoExportOCIFn = install.DefaultSkopeoExportOCI })
	_ = stubHostVarTmpBind(t)

	scratchDir := filepath.Join(tmpDir, "scratch")
	if err := os.MkdirAll(scratchDir, 0755); err != nil {
		t.Fatalf("mkdir scratch: %v", err)
	}
	target := filepath.Join(tmpDir, "target")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}

	// Capture stdout to verify the logged podman command line.
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w

	_ = install.BootcInstall(install.Options{
		ComposeFsBackend: true,
		SourceImgref:     "containers-storage:ghcr.io/projectbluefin/dakota:latest",
		TargetImgref:     "ghcr.io/projectbluefin/dakota:latest",
		Target:           target,
		ScratchDir:       scratchDir,
		NeedsPull:        false,
	})

	w.Close()
	os.Stdout = oldStdout
	var buf bytes.Buffer
	io.Copy(&buf, r) //nolint:errcheck
	output := buf.String()

	ociCacheHost := filepath.Join(scratchDir, "oci-cache")
	const containerOCICachePath = "/run/fisherman/oci-cache"

	// Composefs must bind-mount the OCI cache at the dedicated /run path, not /var/tmp.
	wantMount := ociCacheHost + ":" + containerOCICachePath + ":ro"
	if !strings.Contains(output, wantMount) {
		t.Errorf("podman command missing OCI cache bind-mount %q\ngot: %s", wantMount, output)
	}

	// --source-imgref must point to the container-side path.
	wantSourceImgref := "--source-imgref oci:" + containerOCICachePath
	if !strings.Contains(output, wantSourceImgref) {
		t.Errorf("podman command missing %q\ngot: %s", wantSourceImgref, output)
	}

	// /var/tmp and /tmp must be disk-backed (scratch bind-mount), not a tmpfs
	// — otherwise bootc's internal blob staging fills them and fails with
	// ENOSPC (#20). TMPDIR=/var/tmp forces containers/storage onto /var/tmp.
	scratchMount := scratchDir + ":/var/tmp"
	if !strings.Contains(output, scratchMount) {
		t.Errorf("podman command missing disk-backed scratch mount %q\ngot: %s", scratchMount, output)
	}
	tmpMount := scratchDir + ":/tmp"
	if !strings.Contains(output, tmpMount) {
		t.Errorf("podman command missing disk-backed scratch mount %q\ngot: %s", tmpMount, output)
	}
	if !strings.Contains(output, "-e TMPDIR=/var/tmp") {
		t.Errorf("podman command missing TMPDIR=/var/tmp env var\ngot: %s", output)
	}
	if strings.Contains(output, "--tmpfs /var/tmp") {
		t.Errorf("podman command still uses '--tmpfs /var/tmp' (ENOSPC on multi-GiB images)\ngot: %s", output)
	}
	if strings.Contains(output, "--tmpfs /tmp") {
		t.Errorf("podman command still uses '--tmpfs /tmp' (ENOSPC on multi-GiB images)\ngot: %s", output)
	}
}

// TestComposeFsVsStandardMountSeparation verifies composefs and standard installs
// use different mount strategies and neither panics.
func TestComposeFsVsStandardMountSeparation(t *testing.T) {
	tmpDir := t.TempDir()

	install.SkopeoExportOCIFn = func(image, destDir, tmpdir string) error {
		return os.MkdirAll(destDir, 0755)
	}
	t.Cleanup(func() { install.SkopeoExportOCIFn = install.DefaultSkopeoExportOCI })
	_ = stubHostVarTmpBind(t)

	scratchDir := filepath.Join(tmpDir, "scratch")
	os.MkdirAll(scratchDir, 0755) //nolint:errcheck
	target := filepath.Join(tmpDir, "target")
	os.MkdirAll(target, 0755) //nolint:errcheck

	// Capture composefs podman command.
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	_ = install.BootcInstall(install.Options{
		ComposeFsBackend: true,
		SourceImgref:     "containers-storage:test:latest",
		TargetImgref:     "test:latest",
		Target:           target,
		ScratchDir:       scratchDir,
		NeedsPull:        false,
	})
	w.Close()
	os.Stdout = oldStdout
	var composefsBuf bytes.Buffer
	io.Copy(&composefsBuf, r) //nolint:errcheck
	composefsOut := composefsBuf.String()

	// Composefs must use scratch:/var/tmp so bootc's blob-staging temp dir is
	// disk-backed (ENOSPC fix for #20), and must use the dedicated
	// /run/fisherman/oci-cache path (#38) rather than hiding the cache under
	// /var/tmp/oci-cache. /tmp must also be disk-backed and TMPDIR must point
	// to /var/tmp so containers/storage never falls back to a tmpfs.
	if !strings.Contains(composefsOut, scratchDir+":/var/tmp") {
		t.Errorf("composefs path missing disk-backed scratch:/var/tmp mount (%s)", composefsOut)
	}
	if !strings.Contains(composefsOut, scratchDir+":/tmp") {
		t.Errorf("composefs path missing disk-backed scratch:/tmp mount (%s)", composefsOut)
	}
	if !strings.Contains(composefsOut, "-e TMPDIR=/var/tmp") {
		t.Errorf("composefs path missing TMPDIR=/var/tmp env var (%s)", composefsOut)
	}
	if !strings.Contains(composefsOut, "/run/fisherman/oci-cache") {
		t.Errorf("composefs path missing /run/fisherman/oci-cache: %s", composefsOut)
	}
	if strings.Contains(composefsOut, "--tmpfs /var/tmp") {
		t.Errorf("composefs path uses --tmpfs /var/tmp (causes ENOSPC): %s", composefsOut)
	}
	if strings.Contains(composefsOut, "--tmpfs /tmp") {
		t.Errorf("composefs path uses --tmpfs /tmp (causes ENOSPC): %s", composefsOut)
	}

	// Standard (non-composefs) install should still use scratch:/var/tmp and
	// scratch:/tmp with TMPDIR=/var/tmp.
	r2, w2, _ := os.Pipe()
	os.Stdout = w2
	_ = install.BootcInstall(install.Options{
		ComposeFsBackend: false,
		SourceImgref:     "containers-storage:test:latest",
		TargetImgref:     "test:latest",
		Target:           target,
		ScratchDir:       scratchDir,
	})
	w2.Close()
	os.Stdout = oldStdout
	var standardBuf bytes.Buffer
	io.Copy(&standardBuf, r2) //nolint:errcheck
	standardOut := standardBuf.String()

	if !strings.Contains(standardOut, scratchDir+":/var/tmp") {
		t.Errorf("standard path missing scratch:/var/tmp mount: %s", standardOut)
	}
	if !strings.Contains(standardOut, scratchDir+":/tmp") {
		t.Errorf("standard path missing scratch:/tmp mount: %s", standardOut)
	}
	if !strings.Contains(standardOut, "-e TMPDIR=/var/tmp") {
		t.Errorf("standard path missing TMPDIR=/var/tmp env var: %s", standardOut)
	}
}

// TestBootcViaContainer_MountsSys is a regression test for projectbluefin/fisherman PR #2.
// Without -v /sys:/sys, efibootmgr cannot read or write UEFI variables from
// inside the bootc container, so UEFI boot entries are never updated.
func TestBootcViaContainer_MountsSys(t *testing.T) {
	tmpDir := t.TempDir()

	install.SkopeoExportOCIFn = func(image, destDir, tmpdir string) error { return nil }
	t.Cleanup(func() { install.SkopeoExportOCIFn = install.DefaultSkopeoExportOCI })
	_ = stubHostVarTmpBind(t)

	target := filepath.Join(tmpDir, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w

	_ = install.BootcInstall(install.Options{
		SourceImgref: "containers-storage:ghcr.io/projectbluefin/dakota:latest",
		TargetImgref: "ghcr.io/projectbluefin/dakota:latest",
		Target:       target,
		ScratchDir:   tmpDir,
		NeedsPull:    false,
	})

	w.Close()
	os.Stdout = oldStdout
	var buf bytes.Buffer
	io.Copy(&buf, r) //nolint:errcheck
	output := buf.String()

	if !strings.Contains(output, "-v /sys:/sys") {
		t.Errorf("bootcViaContainer missing '-v /sys:/sys' mount; efibootmgr cannot set UEFI variables without it\ngot: %s", output)
	}
}

// TestBootcToDiskViaContainer_MountsSys is a regression test for projectbluefin/fisherman PR #2.
// Both container-based install paths (bootcViaContainer and bootcToDiskViaContainer)
// must bind-mount /sys so that efibootmgr can write firmware UEFI entries.
func TestBootcToDiskViaContainer_MountsSys(t *testing.T) {
	tmpDir := t.TempDir()

	install.SkopeoExportOCIFn = func(image, destDir, tmpdir string) error { return nil }
	t.Cleanup(func() { install.SkopeoExportOCIFn = install.DefaultSkopeoExportOCI })
	_ = stubHostVarTmpBind(t)

	// BootcToDisk with SourceImgref set routes through bootcToDiskViaContainer.
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w

	_, _ = install.BootcToDisk(install.Options{
		SourceImgref: "containers-storage:ghcr.io/projectbluefin/dakota:latest",
		TargetImgref: "ghcr.io/projectbluefin/dakota:latest",
		ScratchDir:   tmpDir,
	}, "/dev/sda", "ext4")

	w.Close()
	os.Stdout = oldStdout
	var buf bytes.Buffer
	io.Copy(&buf, r) //nolint:errcheck
	output := buf.String()

	if !strings.Contains(output, "-v /sys:/sys") {
		t.Errorf("bootcToDiskViaContainer missing '-v /sys:/sys' mount\ngot: %s", output)
	}
}
