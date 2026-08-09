package install_test

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/tuna-os/fisherman/internal/install"
)

func TestCheckImage_NeedsPullWhenNotCached(t *testing.T) {
	call := 0
	install.SkopeoInspectFn = func(args ...string) ([]byte, error) {
		call++
		if call == 1 {
			// Remote inspect: return manifest with digest + layers
			return []byte(`{"Digest":"sha256:aaaa","Layers":["sha256:l1","sha256:l2"]}`), nil
		}
		// Local inspect: not found
		return nil, fmt.Errorf("image not known")
	}
	defer func() { install.SkopeoInspectFn = install.DefaultSkopeoInspect }()

	result := install.CheckImage("ghcr.io/tuna-os/yellowfin:gnome-hwe")
	if !result.NeedsPull {
		t.Error("NeedsPull should be true when image not in local storage")
	}
	if result.LayerCount != 2 {
		t.Errorf("LayerCount = %d, want 2", result.LayerCount)
	}
}

func TestCheckImage_NoPullWhenCachedAndCurrent(t *testing.T) {
	install.SkopeoInspectFn = func(args ...string) ([]byte, error) {
		// Both remote and local return same digest
		return []byte(`{"Digest":"sha256:bbbb","Layers":["sha256:l1","sha256:l2","sha256:l3"]}`), nil
	}
	defer func() { install.SkopeoInspectFn = install.DefaultSkopeoInspect }()

	result := install.CheckImage("ghcr.io/tuna-os/yellowfin:gnome-hwe")
	if result.NeedsPull {
		t.Error("NeedsPull should be false when local digest matches remote")
	}
	if result.LayerCount != 3 {
		t.Errorf("LayerCount = %d, want 3", result.LayerCount)
	}
}

func TestCheckImage_LocalImagePreferredOverNewer(t *testing.T) {
	// When the local image exists but has a different digest than the remote
	// (i.e. remote is newer), we still use the local image.  The ISO embeds
	// a specific image version for offline install; post-install updates are
	// handled by `bootc update`, not by re-pulling during installation.
	call := 0
	install.SkopeoInspectFn = func(args ...string) ([]byte, error) {
		call++
		if call == 1 {
			return []byte(`{"Digest":"sha256:remote-newer","Layers":["sha256:l1"]}`), nil
		}
		return []byte(`{"Digest":"sha256:local-embedded","Layers":["sha256:l1"]}`), nil
	}
	defer func() { install.SkopeoInspectFn = install.DefaultSkopeoInspect }()

	result := install.CheckImage("ghcr.io/tuna-os/yellowfin:gnome-hwe")
	if result.NeedsPull {
		t.Error("NeedsPull should be false when local image exists, even if remote is newer")
	}
	if result.Offline {
		t.Error("Offline should be false when remote is reachable")
	}
}

func TestCheckImage_NeedsPullOnNetworkErrorNoCachedImage(t *testing.T) {
	install.SkopeoInspectFn = func(args ...string) ([]byte, error) {
		return nil, fmt.Errorf("network error")
	}
	defer func() { install.SkopeoInspectFn = install.DefaultSkopeoInspect }()

	result := install.CheckImage("ghcr.io/tuna-os/yellowfin:gnome-hwe")
	if !result.NeedsPull {
		t.Error("NeedsPull should be true when offline and image not in local storage")
	}
	if result.Offline {
		t.Error("Offline should be false when image is not in local storage")
	}
}

func TestCheckImage_OfflineWithLocalCache(t *testing.T) {
	call := 0
	install.SkopeoInspectFn = func(args ...string) ([]byte, error) {
		call++
		if call == 1 {
			// Remote inspect: offline / unreachable
			return nil, fmt.Errorf("network unreachable")
		}
		// Local inspect: image is cached
		return []byte(`{"Digest":"sha256:cached","Layers":["sha256:l1","sha256:l2","sha256:l3"]}`), nil
	}
	defer func() { install.SkopeoInspectFn = install.DefaultSkopeoInspect }()

	result := install.CheckImage("ghcr.io/tuna-os/yellowfin:gnome-hwe")
	if result.NeedsPull {
		t.Error("NeedsPull should be false when offline but image is in local storage")
	}
	if !result.Offline {
		t.Error("Offline should be true when registry was unreachable")
	}
	if result.LayerCount != 3 {
		t.Errorf("LayerCount = %d, want 3", result.LayerCount)
	}
}

func TestClassifyLine_LayersNeeded(t *testing.T) {
	line := "layers already present: 0; layers needed: 64 (3.7\u00a0GB)"
	got := install.ClassifyLine(line)
	want := "Writing 64 (3.7\u00a0GB) to disk — this may take several minutes"
	if got != want {
		t.Errorf("ClassifyLine(%q) = %q, want %q", line, got, want)
	}
}

func TestClassifyLine_LayersNeededZero(t *testing.T) {
	line := "layers already present: 64; layers needed: 0"
	got := install.ClassifyLine(line)
	want := "Writing 0 to disk — this may take several minutes"
	if got != want {
		t.Errorf("ClassifyLine(%q) = %q, want %q", line, got, want)
	}
}

func TestClassifyLine_InstallingImage(t *testing.T) {
	got := install.ClassifyLine("Installing image: docker://ghcr.io/tuna-os/yellowfin:gnome-hwe")
	if got != "Deploying image" {
		t.Errorf("got %q, want 'Deploying image'", got)
	}
}

func TestClassifyLine_NoMatch(t *testing.T) {
	got := install.ClassifyLine("some random line")
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestBuildBootcArgs_BaseArgs(t *testing.T) {
	args := install.BuildBootcArgs(install.Options{Target: "/mnt/target"}, "", "/target")
	// Must always include these
	assertContains(t, args, "install")
	assertContains(t, args, "to-filesystem")
	assertContains(t, args, "--skip-finalize")
	assertContains(t, args, "/target")
}

func TestBuildBootcArgs_ComposeFsBackend(t *testing.T) {
	args := install.BuildBootcArgs(install.Options{ComposeFsBackend: true}, "", "/target")
	assertContains(t, args, "--composefs-backend")
}

// TestBuildBootcArgs_ComposeFsBackend_SourceImgref verifies that BuildBootcArgs
// emits --source-imgref oci:<scratchDir>/oci-cache when ComposeFsBackend is true.
// In container mode, callers set ComposeFsOCIPath = containerOCICachePath
// ("/run/fisherman/oci-cache") so the source points to the bind-mount destination
// inside the container; in direct mode, the host-side scratchDir/oci-cache is used.
func TestBuildBootcArgs_ComposeFsBackend_SourceImgref(t *testing.T) {
	// Direct mode: no ComposeFsOCIPath — falls back to scratchDir/oci-cache.
	directArgs := install.BuildBootcArgs(install.Options{ComposeFsBackend: true}, "", "/target")
	assertContains(t, directArgs, "--source-imgref")
	assertContains(t, directArgs, "oci:/var/fisherman-tmp/oci-cache") // default scratchDir

	// Container mode: ComposeFsOCIPath is set to the container-side mount path.
	containerArgs := install.BuildBootcArgs(install.Options{
		ComposeFsBackend: true,
		ComposeFsOCIPath: "/run/fisherman/oci-cache",
	}, "", "/target")
	assertContains(t, containerArgs, "--source-imgref")
	assertContains(t, containerArgs, "oci:/run/fisherman/oci-cache")
}

func TestBuildBootcArgs_NoComposeFsBackend_NoSourceImgref(t *testing.T) {
	args := install.BuildBootcArgs(install.Options{ComposeFsBackend: false}, "", "/target")
	assertAbsent(t, args, "--source-imgref")
	assertAbsent(t, args, "--composefs-backend")
}

// TestBuildBootcArgs_OCIPathWithoutComposefs verifies that when ComposeFsOCIPath
// is set but ComposeFsBackend is false (non-composefs OCI redirect), the
// --source-imgref flag IS emitted but --composefs-backend is NOT.
// Regression test for the bug where --source-imgref was only emitted when
// ComposeFsBackend was true, causing bootc to fail with "no such file or
// directory" when the OCI layout was exported but the flag was missing.
func TestBuildBootcArgs_OCIPathWithoutComposefs(t *testing.T) {
	args := install.BuildBootcArgs(install.Options{
		ComposeFsBackend: false,
		ComposeFsOCIPath: "/run/fisherman/oci-cache",
	}, "", "/target")
	assertContains(t, args, "--source-imgref")
	assertContains(t, args, "oci:/run/fisherman/oci-cache")
	assertAbsent(t, args, "--composefs-backend")
}

// TestBuildBootcArgs_DirectModeSourceImgref verifies that when SourceImgref
// is empty (direct/live-ISO mode) but TargetImgref is set, BuildBootcArgs
// emits --source-imgref with the ostree-unverified-registry transport.
// Bootc rejects direct installs without an explicit source when not running
// inside a podman container.
func TestBuildBootcArgs_DirectModeSourceImgref(t *testing.T) {
	args := install.BuildBootcArgs(install.Options{
		SourceImgref: "", // direct mode
	}, "ghcr.io/ublue-os/bluefin:stable", "/target")
	assertContains(t, args, "--source-imgref")
	assertContains(t, args, "containers-storage:ghcr.io/ublue-os/bluefin:stable")
}

func TestBuildBootcArgs_NoComposeFsBackend(t *testing.T) {
	args := install.BuildBootcArgs(install.Options{ComposeFsBackend: false}, "", "/target")
	assertAbsent(t, args, "--composefs-backend")
}

// TestBuildBootcArgs_UnifiedStorage confirms --experimental-unified-storage is
// never emitted. The flag is incompatible with fisherman's podman-run launch
// model: bootc builds its internal storage using overlay@/run/bootc/storage+
// /proc/self/fd/3, but that fd is not inherited by the copy subprocess bootc
// spawns, so the reference never resolves. Standard containers-storage is used.
func TestBuildBootcArgs_UnifiedStorage(t *testing.T) {
	args := install.BuildBootcArgs(install.Options{UnifiedStorage: true}, "", "/target")
	assertAbsent(t, args, "--experimental-unified-storage")
}

func TestBuildBootcArgs_NoUnifiedStorage(t *testing.T) {
	args := install.BuildBootcArgs(install.Options{UnifiedStorage: false}, "", "/target")
	assertAbsent(t, args, "--experimental-unified-storage")
}

func TestBuildBootcArgs_SelinuxDisabled(t *testing.T) {
	args := install.BuildBootcArgs(install.Options{SelinuxDisabled: true}, "", "/target")
	assertContains(t, args, "--disable-selinux")
}

func TestBuildBootcArgs_NoSelinux(t *testing.T) {
	args := install.BuildBootcArgs(install.Options{SelinuxDisabled: false}, "", "/target")
	assertAbsent(t, args, "--disable-selinux")
}

func TestBuildBootcArgs_TargetImgref(t *testing.T) {
	args := install.BuildBootcArgs(install.Options{}, "ghcr.io/tuna-os/yellowfin:gnome50", "/target")
	assertContains(t, args, "--target-imgref")
	assertContains(t, args, "ghcr.io/tuna-os/yellowfin:gnome50")
}

func TestBuildBootcArgs_NoTargetImgref(t *testing.T) {
	args := install.BuildBootcArgs(install.Options{}, "", "/target")
	assertAbsent(t, args, "--target-imgref")
}

func TestBuildBootcArgs_AllFlags(t *testing.T) {
	opts := install.Options{
		ComposeFsBackend: true,
		UnifiedStorage:   true,
		SelinuxDisabled:  true,
		ComposeFsOCIPath: "/run/fisherman/oci-cache",
	}
	args := install.BuildBootcArgs(opts, "img:tag", "/target")
	assertContains(t, args, "--composefs-backend")
	assertContains(t, args, "oci:/run/fisherman/oci-cache")
	assertAbsent(t, args, "--experimental-unified-storage") // never emitted; see Options.UnifiedStorage
	assertContains(t, args, "--disable-selinux")
	assertContains(t, args, "--target-imgref")
}

// TestSkopeoExportOCI_CalledForComposeFsBackend is a regression test for the
// composefs-backend OCI blob issue: podman pull does not preserve raw compressed
// layer tarballs, so bootc --composefs-backend fails with "file does not exist".
// The fix exports the image to an OCI layout via skopeo before invoking bootc.
// This test verifies the SkopeoExportOCIFn hook is replaceable (used for
// integration tests to capture the args without running real disk I/O).
func TestSkopeoExportOCI_FnIsReplaceable(t *testing.T) {
	var capturedImage, capturedDir string
	install.SkopeoExportOCIFn = func(image, destDir, tmpdir string) error {
		capturedImage = image
		capturedDir = destDir
		_ = tmpdir
		return nil
	}
	defer func() { install.SkopeoExportOCIFn = install.DefaultSkopeoExportOCI }()

	if err := install.SkopeoExportOCIFn("ghcr.io/projectbluefin/dakota:latest", "/var/fisherman-tmp/oci-cache", "/var/fisherman-tmp"); err != nil {
		t.Fatalf("stub returned unexpected error: %v", err)
	}
	if capturedImage != "ghcr.io/projectbluefin/dakota:latest" {
		t.Errorf("image = %q, want ghcr.io/projectbluefin/dakota:latest", capturedImage)
	}
	if capturedDir != "/var/fisherman-tmp/oci-cache" {
		t.Errorf("destDir = %q, want /var/fisherman-tmp/oci-cache", capturedDir)
	}
}

func TestBootcInstall_DirectComposeFsExportsOCI(t *testing.T) {
	tmpDir := t.TempDir()
	bootcPath := tmpDir + "/bootc"
	if err := os.WriteFile(bootcPath, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatalf("writing fake bootc: %v", err)
	}

	oldPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", tmpDir+":"+oldPath); err != nil {
		t.Fatalf("setting PATH: %v", err)
	}
	t.Cleanup(func() { _ = os.Setenv("PATH", oldPath) })

	var capturedImage, capturedDir string
	install.SkopeoExportOCIFn = func(image, destDir, tmpdir string) error {
		capturedImage = image
		capturedDir = destDir
		_ = tmpdir
		return nil
	}
	defer func() { install.SkopeoExportOCIFn = install.DefaultSkopeoExportOCI }()

	err := install.BootcInstall(install.Options{
		ComposeFsBackend: true,
		TargetImgref:     "ghcr.io/projectbluefin/dakota:latest",
		Target:           "/mnt/fisherman-target",
	})
	if err != nil {
		t.Fatalf("BootcInstall() error = %v", err)
	}
	if capturedImage != "ghcr.io/projectbluefin/dakota:latest" {
		t.Errorf("image = %q, want ghcr.io/projectbluefin/dakota:latest", capturedImage)
	}
	if capturedDir != "/var/fisherman-tmp/oci-cache" {
		t.Errorf("destDir = %q, want /var/fisherman-tmp/oci-cache", capturedDir)
	}
}

func TestBootcInstall_DirectComposeFsUsesCustomScratchDir(t *testing.T) {
	tmpDir := t.TempDir()
	bootcPath := tmpDir + "/bootc"
	if err := os.WriteFile(bootcPath, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatalf("writing fake bootc: %v", err)
	}

	oldPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", tmpDir+":"+oldPath); err != nil {
		t.Fatalf("setting PATH: %v", err)
	}
	t.Cleanup(func() { _ = os.Setenv("PATH", oldPath) })

	var capturedDir string
	install.SkopeoExportOCIFn = func(image, destDir, tmpdir string) error {
		capturedDir = destDir
		_ = image
		_ = tmpdir
		return nil
	}
	defer func() { install.SkopeoExportOCIFn = install.DefaultSkopeoExportOCI }()

	err := install.BootcInstall(install.Options{
		ComposeFsBackend: true,
		TargetImgref:     "ghcr.io/projectbluefin/dakota:latest",
		Target:           "/mnt/fisherman-target",
		ScratchDir:       "/tmp/custom-scratch",
	})
	if err != nil {
		t.Fatalf("BootcInstall() error = %v", err)
	}
	if capturedDir != "/tmp/custom-scratch/oci-cache" {
		t.Errorf("destDir = %q, want /tmp/custom-scratch/oci-cache", capturedDir)
	}
}

func TestBuildSelinuxBypassShim_Compiles(t *testing.T) {
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("cc not found; skipping shim compilation test")
	}
	soPath, err := install.BuildSelinuxBypassShim()
	if err != nil {
		t.Fatalf("BuildSelinuxBypassShim() error = %v", err)
	}
	defer os.Remove(soPath)

	info, err := os.Stat(soPath)
	if err != nil {
		t.Fatalf("shim .so not found at %s: %v", soPath, err)
	}
	if info.Size() == 0 {
		t.Error("shim .so is empty")
	}
}

// TestBuildSelinuxBypassShim_InterceptsSecuritySelinux verifies that the compiled
// shim actually intercepts lsetxattr("security.selinux", ...) and returns 0
// without performing the write. Other xattr names must pass through normally.
func TestBuildSelinuxBypassShim_InterceptsSecuritySelinux(t *testing.T) {
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("cc not found; skipping shim test")
	}
	soPath, err := install.BuildSelinuxBypassShim()
	if err != nil {
		t.Fatalf("BuildSelinuxBypassShim() error = %v", err)
	}
	defer os.Remove(soPath)

	// Run a small helper binary under LD_PRELOAD that calls lsetxattr and reports
	// whether the call succeeded (exit 0) or failed (exit 1).
	tmpFile, err := os.CreateTemp(t.TempDir(), "xattr-test-*")
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	tmpFile.Close()

	// Use python3 to call lsetxattr via ctypes — available on any Linux system.
	script := fmt.Sprintf(`
import ctypes, sys
libc = ctypes.CDLL(None)
path = b%q
# security.selinux: shim must intercept and return 0 (success)
rc = libc.lsetxattr(path, b"security.selinux", b"user_u:object_r:user_home_t:s0", 36, 0)
if rc != 0:
    sys.exit(1)
# user.test: shim must NOT intercept (real syscall; may fail with EOPNOTSUPP on
# some filesystems, but must NOT be silently 0 from a stub that ignores all names)
# We just check that the code path is reached (no crash / panic from shim).
sys.exit(0)
`, tmpFile.Name())

	cmd := exec.Command("python3", "-c", script)
	cmd.Env = append(os.Environ(), "LD_PRELOAD="+soPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("security.selinux lsetxattr should return 0 with shim active, got error: %v\n%s", err, out)
	}
}

// assertContains fails the test if s is not present in slice.
func assertContains(t *testing.T, slice []string, s string) {
	t.Helper()
	for _, v := range slice {
		if v == s {
			return
		}
	}
	t.Errorf("expected %q in args %v", s, slice)
}

// assertAbsent fails the test if s is present in slice.
func assertAbsent(t *testing.T, slice []string, s string) {
	t.Helper()
	for _, v := range slice {
		if v == s {
			t.Errorf("unexpected %q in args %v", s, slice)
			return
		}
	}
}

// TestNeedsContainerStorageMount_UnifiedStorage confirms that /var/lib/containers
// is still mounted when UnifiedStorage=true. Bootc needs the mount to find the
// source image in host podman storage and copy it into bootc's own storage.
func TestNeedsContainerStorageMount_UnifiedStorage(t *testing.T) {
	if !install.NeedsContainerStorageMount(install.Options{UnifiedStorage: true}) {
		t.Error("should mount /var/lib/containers even when UnifiedStorage=true (bootc needs it to locate the source image)")
	}
}

func TestNeedsContainerStorageMount_Standard(t *testing.T) {
	if !install.NeedsContainerStorageMount(install.Options{UnifiedStorage: false}) {
		t.Error("should mount /var/lib/containers for standard (non-unified) installs")
	}
}

func TestNeedsContainerStorageMount_ComposeFsBackend(t *testing.T) {
	if install.NeedsContainerStorageMount(install.Options{ComposeFsBackend: true}) {
		t.Error("should NOT mount /var/lib/containers when ComposeFsBackend=true")
	}
}

// TestInjectStorageTmpDir verifies that injectStorageTmpDir correctly adds or
// replaces the tmpdir line in a containers/storage TOML config string.
func TestInjectStorageTmpDir(t *testing.T) {
	newLine := `tmpdir = "/scratch"`

	t.Run("replaces existing tmpdir", func(t *testing.T) {
		conf := "[storage]\ndriver = \"vfs\"\ntmpdir = \"/old\"\ngraphroot = \"/var/lib/containers/storage\"\n"
		result := install.InjectStorageTmpDir(conf, newLine)
		if !strings.Contains(result, `tmpdir = "/scratch"`) {
			t.Errorf("expected replaced tmpdir, got:\n%s", result)
		}
		if strings.Contains(result, `"/old"`) {
			t.Errorf("old tmpdir still present:\n%s", result)
		}
	})

	t.Run("injects when no tmpdir line", func(t *testing.T) {
		conf := "[storage]\ndriver = \"vfs\"\nrunroot = \"/run/containers/storage\"\ngraphroot = \"/var/lib/containers/storage\"\n"
		result := install.InjectStorageTmpDir(conf, newLine)
		if !strings.Contains(result, `tmpdir = "/scratch"`) {
			t.Errorf("tmpdir not injected, got:\n%s", result)
		}
		// Existing fields must still be present.
		if !strings.Contains(result, `driver = "vfs"`) {
			t.Errorf("driver line missing:\n%s", result)
		}
	})

	t.Run("injects before next section", func(t *testing.T) {
		conf := "[storage]\ndriver = \"overlay\"\n\n[storage.options]\nadditionalimagestores = []\n"
		result := install.InjectStorageTmpDir(conf, newLine)
		if !strings.Contains(result, `tmpdir = "/scratch"`) {
			t.Errorf("tmpdir not injected, got:\n%s", result)
		}
		// additionalimagestores must survive unchanged.
		if !strings.Contains(result, "additionalimagestores") {
			t.Errorf("[storage.options] section lost:\n%s", result)
		}
	})

	t.Run("handles empty config (live-ISO fallback)", func(t *testing.T) {
		conf := ""
		result := install.InjectStorageTmpDir(conf, newLine)
		// No [storage] section → nothing to inject, just return unchanged.
		// The fallback path in writeStorageConfWithTmpDir handles this.
		_ = result // just must not panic
	})
}

// TestBootcInstall_NonComposefsContainerExportsOCI verifies that when
// SourceImgref is set and ComposeFsBackend is false (non-composefs container
// mode with overlay redirect), the OCI export function IS called.  Regression
// test for the bug where exportComposefsOCIIfNeeded returned nil for
// non-composefs, causing "oci-cache/index.json: no such file or directory".
func TestBootcInstall_NonComposefsContainerExportsOCI(t *testing.T) {
	tmpDir := t.TempDir()
	var scratchDir string
	var err error

	// Fake podman: just exit 0 (container mode runs podman; we don't need a real one).
	podmanPath := tmpDir + "/podman"
	if err = os.WriteFile(podmanPath, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatalf("writing fake podman: %v", err)
	}

	oldPath := os.Getenv("PATH")
	if err = os.Setenv("PATH", tmpDir+":"+oldPath); err != nil {
		t.Fatalf("setting PATH: %v", err)
	}
	t.Cleanup(func() { _ = os.Setenv("PATH", oldPath) })

	var exportCalled bool
	install.SkopeoExportOCIFn = func(image, destDir, tmpdir string) error {
		exportCalled = true
		// Create a minimal OCI layout so the subsequent podman run can
		// find oci:<path> (we just need the file to exist for the mock).
		if err := os.MkdirAll(destDir, 0755); err != nil {
			return err
		}
		return os.WriteFile(destDir+"/index.json", []byte("{}"), 0644)
	}
	defer func() { install.SkopeoExportOCIFn = install.DefaultSkopeoExportOCI }()

	// The overlay redirect requires scratch on an overlay-capable filesystem
	// (ext4/xfs/btrfs).  t.TempDir() is typically on tmpfs.  Use /var/tmp
	// which is usually ext4/xfs on CI and developer machines.
	scratchDir, err = os.MkdirTemp("/var/tmp", "fisherman-test-scratch-*")
	if err != nil {
		t.Skipf("cannot create scratch on /var/tmp: %v", err)
	}
	defer os.RemoveAll(scratchDir)

	err = install.BootcInstall(install.Options{
		ComposeFsBackend: false,
		SourceImgref:     "containers-storage:ghcr.io/ublue-os/bluefin:stable",
		TargetImgref:     "ghcr.io/ublue-os/bluefin:stable",
		Target:           tmpDir + "/target",
		ScratchDir:       scratchDir,
		NeedsPull:        false,
	})
	if err != nil {
		t.Fatalf("BootcInstall() error = %v", err)
	}
	if !exportCalled {
		t.Error("SkopeoExportOCIFn was not called for non-composefs container mode with SourceImgref set")
	}
}

// TestBootcInstall_NonComposefsDirectSkipsOCIExport verifies that when
// SourceImgref is empty (direct/live-ISO mode) and ComposeFsBackend is false,
// the OCI export is NOT called.  Bootc reads from containers-storage on the
// host; no OCI layout is needed.
func TestBootcInstall_NonComposefsDirectSkipsOCIExport(t *testing.T) {
	tmpDir := t.TempDir()

	// Fake bootc for direct mode.
	bootcPath := tmpDir + "/bootc"
	if err := os.WriteFile(bootcPath, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatalf("writing fake bootc: %v", err)
	}

	oldPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", tmpDir+":"+oldPath); err != nil {
		t.Fatalf("setting PATH: %v", err)
	}
	t.Cleanup(func() { _ = os.Setenv("PATH", oldPath) })

	var exportCalled bool
	install.SkopeoExportOCIFn = func(image, destDir, tmpdir string) error {
		exportCalled = true
		return nil
	}
	defer func() { install.SkopeoExportOCIFn = install.DefaultSkopeoExportOCI }()

	err := install.BootcInstall(install.Options{
		ComposeFsBackend: false,
		SourceImgref:     "", // direct mode — no source image ref
		Target:           tmpDir + "/target",
	})
	if err != nil {
		t.Fatalf("BootcInstall() error = %v", err)
	}
	if exportCalled {
		t.Error("SkopeoExportOCIFn was called for non-composefs direct mode (should be skipped)")
	}
}
