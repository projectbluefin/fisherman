package install

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tuna-os/fisherman/internal/progress"
	"github.com/tuna-os/fisherman/internal/runner"
)

// selinuxBypassCSrc is a minimal LD_PRELOAD shim that silently succeeds
// for security.selinux xattr writes. It is compiled at runtime and injected
// into the bootc container when installing a cross-distro image (e.g.
// AlmaLinux) on a host running a different SELinux policy (e.g. Fedora).
//
// Without the shim, two failures occur:
//  1. bootc's imgstorage calls lsetfilecon(), which calls lsetxattr() with
//     "security.selinux" labels from the image. The host kernel rejects
//     unknown types (EINVAL) because they don't exist in the loaded policy.
//  2. libostree writes raw "security.selinux" xattrs from the OCI layer
//     metadata via fsetxattr(). Same EINVAL from the kernel.
//
// The shim intercepts both syscalls and returns 0, so the install proceeds
// without SELinux labels. Since the target system has selinux=disabled, the
// missing labels are harmless.
const selinuxBypassCSrc = `
#define _GNU_SOURCE
#include <dlfcn.h>
#include <sys/xattr.h>
#include <string.h>

int lsetxattr(const char *path, const char *name, const void *value, size_t size, int flags) {
	if (name && strcmp(name, "security.selinux") == 0) return 0;
	static int (*real)(const char*,const char*,const void*,size_t,int);
	if (!real) real = dlsym(RTLD_NEXT, "lsetxattr");
	return real(path, name, value, size, flags);
}

int fsetxattr(int fd, const char *name, const void *value, size_t size, int flags) {
	if (name && strcmp(name, "security.selinux") == 0) return 0;
	static int (*real)(int,const char*,const void*,size_t,int);
	if (!real) real = dlsym(RTLD_NEXT, "fsetxattr");
	return real(fd, name, value, size, flags);
}
`

// BuildSelinuxBypassShim compiles selinuxBypassCSrc into a shared library
// at /tmp/fisherman-selinux-bypass.so and returns its path.
// Returns an error if cc is not available or compilation fails.
func BuildSelinuxBypassShim() (string, error) {
	const (
		srcPath = "/tmp/fisherman-selinux-bypass.c"
		soPath  = "/tmp/fisherman-selinux-bypass.so"
	)
	if err := os.WriteFile(srcPath, []byte(selinuxBypassCSrc), 0644); err != nil {
		return "", fmt.Errorf("writing shim source: %w", err)
	}
	defer os.Remove(srcPath)

	out, err := exec.Command("cc", "-shared", "-fPIC", "-O2", "-nostartfiles", "-ldl",
		"-o", soPath, srcPath).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("compiling SELinux bypass shim: %w\n%s", err, out)
	}
	if err := os.Chmod(soPath, 0755); err != nil {
		return "", fmt.Errorf("chmod shim: %w", err)
	}
	return soPath, nil
}

// Options configures a bootc installation.
type Options struct {
	// SourceImgref is the OCI image to run bootc from (e.g. "quay.io/tuna-os/yellowfin:gnome-hwe").
	// When empty, fisherman is assumed to already be running inside the container image
	// (live-ISO mode) and bootc is called directly.
	// When set, bootc is invoked via `podman run --privileged <image> bootc install to-filesystem`
	// per the bootc requirement that install must run FROM the container image.
	SourceImgref string
	// TargetImgref is the update-tracking reference written into the installed system
	// for day-2 updates (--target-imgref). If empty, defaults to SourceImgref.
	TargetImgref string
	// SelinuxDisabled passes --disable-selinux when true.
	SelinuxDisabled bool
	// UnifiedStorage is retained in the schema for forwards compatibility but
	// is NOT emitted as a flag. --experimental-unified-storage requires bootc
	// to run on bare metal; fisherman always runs bootc inside `podman run
	// --privileged`, where bootc builds its internal storage using
	// overlay@/run/bootc/storage+/proc/self/fd/3. The fd is not inherited by
	// the copy subprocess bootc spawns, so the reference never resolves and
	// the install fails. Standard containers-storage (via the /var/lib/containers
	// bind-mount) is used instead. See: https://bootc.dev/bootc/experimental-unified-storage.html
	UnifiedStorage bool
	// ComposeFsBackend passes --composefs-backend when true.
	// Required for images using the composefs-native deployment backend (e.g. ghcr.io/bootcrew/*).
	ComposeFsBackend bool
	// Bootloader selects the bootloader passed to bootc via --bootloader.
	// Empty or "grub2" uses the default (grub2). "systemd" passes --bootloader systemd.
	Bootloader string
	// Target is the path to the mounted root filesystem on the host.
	Target string
	// ScratchDir is the host-side directory used for temporary I/O during
	// installation (OCI exports, layer blobs, etc.). It is bind-mounted at
	// /var/tmp by the caller. Defaults to "/var/fisherman-tmp" when empty.
	ScratchDir string
	// NeedsPull is the result of a pre-flight CheckImage call. When false,
	// the image pull is skipped (image already in containers-storage).
	NeedsPull bool
	// LayerCount is the number of image layers from CheckImage, used to
	// show "layer N/total" progress. 0 means unknown.
	LayerCount int
	// AdditionalImageStores is a list of host paths to expose to the bootc
	// container as containers/storage additionalimagestores. Used for
	// offline image stores (e.g. squashfs on a live ISO). When the caller has
	// already set CONTAINERS_STORAGE_CONF, that takes priority and this list
	// is ignored.
	AdditionalImageStores []string
	// ComposeFsOCIPath is the container-side path passed to bootc as
	// --source-imgref oci:<path>. Set by bootcViaContainer to the bind-mount
	// destination inside the container (e.g. /run/fisherman/oci-cache).
	// When empty, BuildBootcArgs falls back to the host-side OCI cache
	// (scratchDir/oci-cache), which is correct for bootcDirect (no container).
	ComposeFsOCIPath string
}

// scratchDir returns the host-side scratch directory from opts, falling back
// to the default "/var/fisherman-tmp" when unset.
func (o Options) scratchDir() string {
	if o.ScratchDir != "" {
		return o.ScratchDir
	}
	return "/var/fisherman-tmp"
}

// containerOCICachePath is the container-side path where the OCI cache is
// bind-mounted when running bootc via a podman container. The mount lives at a
// dedicated /run/fisherman path rather than under /var/tmp — /var/tmp is bound
// to the disk-backed scratch dir (so bootc's blob staging never hits a tmpfs,
// see #20) and keeping the cache separate avoids the nested-mount issues on
// btrfs-on-LUKS that made the cache invisible under /var/tmp (see #38).
const containerOCICachePath = "/run/fisherman/oci-cache"

// varTmpOverrideDir returns the scratch subdir that is bind-mounted over the
// HOST's /var/tmp for the duration of an install. containers/image hardcodes
// /var/tmp for multi-GiB blob staging (internal/tmpdir.unixTempDirForBigFiles,
// reachable only via SystemContext.BigFilesTemporaryDir, which bootc's skopeo
// subprocess never sets), and bootc's install preflight re-mirrors the
// container's /var/tmp from the HOST's (ensure_mirrored_host_mount, comparing
// statvfs f_fsid against /proc/1/root/var/tmp via the host PID namespace). On a
// live ISO the host /var/tmp is the tiny dracut overlay (~1.4 GiB), so blob
// staging there fails with ENOSPC (#20/#21). Without this bind, no storage.conf
// tmpdir, $TMPDIR, or container-side mount survives those two mechanisms.
func varTmpOverrideDir(scratch string) string {
	return filepath.Join(scratch, "var-tmp-override")
}

// hostVarTmpBound reports whether /var/tmp is currently a bind mount of
// override. A bind mount of a directory presents the source's device and
// inode, so os.SameFile comparing the two stats is the idempotence check that
// lets both the whole-install binder and skopeoExportOCI share one mount
// without stacking.
func hostVarTmpBound(override string) bool {
	vt, err1 := os.Stat("/var/tmp")
	ov, err2 := os.Stat(override)
	if err1 != nil || err2 != nil {
		return false
	}
	return os.SameFile(vt, ov)
}

// DefaultHostVarTmpBind bind-mounts the disk-backed override directory over
// the HOST's /var/tmp, in the host mount namespace (runner.HostArgs goes
// through flatpak-spawn --host when inside a Flatpak sandbox, and /proc/1/root
// resolves in that same namespace). It must be held for the whole install:
// bootc re-mirrors the container's /var/tmp from the host's, so making the host
// /var/tmp disk-backed is what keeps bootc's blob staging off the tiny dracut
// overlay on a live ISO. The returned cleanup unmounts /var/tmp; it is a no-op
// when the override was already bound.
//
// Overridable via HostVarTmpBindFn in tests.
func DefaultHostVarTmpBind(scratch string) (func(), error) {
	override := varTmpOverrideDir(scratch)
	if err := os.MkdirAll(override, 0o1777); err != nil {
		return nil, err
	}
	if hostVarTmpBound(override) {
		fmt.Fprintf(os.Stdout, "# host /var/tmp already bind-mounted → %s\n", override)
		return func() {}, nil
	}
	mntName, mntArgs := runner.HostArgs("mount", []string{"--bind", override, "/var/tmp"})
	if err := exec.Command(mntName, mntArgs...).Run(); err != nil {
		return nil, fmt.Errorf("mounting %s over host /var/tmp: %w", override, err)
	}
	fmt.Fprintf(os.Stdout, "# host /var/tmp bind-mounted → %s (disk-backed) for the install\n", override)
	return func() {
		umName, umArgs := runner.HostArgs("umount", []string{"/var/tmp"})
		_ = exec.Command(umName, umArgs...).Run()
	}, nil
}

// HostVarTmpBindFn ensures the HOST /var/tmp is disk-backed for the duration of
// a bootc install. bootc's install preflight replaces the container's /var/tmp
// with the host's, so this bind (not any container-side mount) is what prevents
// ENOSPC during blob staging on live ISOs. Replace in tests to avoid real mount
// syscalls. A nil cleanup means nothing was bound.
var HostVarTmpBindFn = DefaultHostVarTmpBind

// BuildBootcArgs builds the argument slice for `bootc install to-filesystem`.
// resolvedTargetImgref is the --target-imgref value (empty to omit the flag).
// installTarget is the final positional argument (e.g. "/target" in container mode,
// or opts.Target in direct mode).
func BuildBootcArgs(opts Options, resolvedTargetImgref, installTarget string) []string {
	args := []string{"install", "to-filesystem"}
	if resolvedTargetImgref != "" {
		args = append(args, "--target-imgref", resolvedTargetImgref)
	}
	if opts.SelinuxDisabled {
		args = append(args, "--disable-selinux")
	}
	// UnifiedStorage is intentionally not emitted — see Options.UnifiedStorage comment.
	if opts.ComposeFsBackend {
		args = append(args, "--composefs-backend")
	}
	// --source-imgref is required for composefs (raw OCI blobs), for
	// non-composefs OCI-redirect installs, and for direct mode where bootc
	// is not running inside a podman container.
	ociPath := opts.ComposeFsOCIPath
	if ociPath == "" {
		ociPath = opts.scratchDir() + "/oci-cache"
	}
	if opts.ComposeFsBackend || opts.ComposeFsOCIPath != "" {
		args = append(args, "--source-imgref", "oci:"+ociPath)
	} else if resolvedTargetImgref != "" && opts.SourceImgref == "" {
		// Direct mode: bootc runs natively (not in a container) and needs
		// an explicit --source-imgref.  Use containers-storage transport
		// to read the image from local storage (embedded in the live ISO
		// at /usr/lib/containers/storage).  No network pull needed.
		args = append(args, "--source-imgref", "containers-storage:"+bareImageRef(resolvedTargetImgref))
	}
	if opts.Bootloader != "" && opts.Bootloader != "grub2" {
		args = append(args, "--bootloader", opts.Bootloader)
	}
	args = append(args, "--skip-finalize")
	args = append(args, installTarget)
	return args
}

// selinuxActive reports whether the host kernel has the SELinux security module
// loaded and active. The presence of /sys/fs/selinux/enforce is the standard
// indicator used by libselinux's is_selinux_enabled().
func selinuxActive() bool {
	_, err := os.Stat("/sys/fs/selinux/enforce")
	return err == nil
}

// NeedsContainerStorageMount reports whether the podman run invocation for a
// bootc install should bind-mount /var/lib/containers into the container.
// Required for all standard installs so bootc can find the source image layers.
// Skipped only for composefs, which uses an OCI layout exported to /var/tmp.
func NeedsContainerStorageMount(opts Options) bool {
	return !opts.ComposeFsBackend
}

// writeAdditionalStoresConf writes a containers/storage config that lists
// every path in stores under additionalimagestores. The file is created under
// scratchDir/fisherman-conf/ (not at scratchDir root, where it would be mixed
// in with the OCI cache). scratchDir is bind-mounted as /var/tmp inside the
// bootc container, so the container-side path is /var/tmp/fisherman-conf/<name>.
//
// Returns the host-side path (for cleanup) and the container-side path
// (for the CONTAINERS_STORAGE_CONF env var).
func writeAdditionalStoresConf(scratchDir string, stores []string) (hostPath, containerPath string, err error) {
	confDir := filepath.Join(scratchDir, "fisherman-conf")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return "", "", err
	}
	quoted := make([]string, len(stores))
	for i, s := range stores {
		// Escape backslashes and double-quotes to prevent TOML injection.
		escaped := strings.ReplaceAll(s, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		escaped = strings.ReplaceAll(escaped, "\n", "")
		quoted[i] = `"` + escaped + `"`
	}
	conf := "[storage]\ndriver = \"overlay\"\n\n[storage.options]\nadditionalimagestores = [" +
		strings.Join(quoted, ", ") + "]\n"
	f, err := os.CreateTemp(confDir, "storage-*.conf")
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	if _, err := f.WriteString(conf); err != nil {
		os.Remove(f.Name())
		return "", "", err
	}
	contPath := "/var/tmp/fisherman-conf/" + filepath.Base(f.Name())
	return f.Name(), contPath, nil
}

// appendImageStoreArgs adds the podman flags needed to make additional OCI
// image stores visible inside the bootc container:
//
//   - Each host path in opts.AdditionalImageStores is bind-mounted read-only
//     at the same path inside the container so paths in storage.conf resolve.
//   - A fisherman-generated storage.conf listing those paths under
//     additionalimagestores is written into scratch and passed via
//     CONTAINERS_STORAGE_CONF.
//
// If the caller has already set CONTAINERS_STORAGE_CONF in the environment,
// it takes priority: the file is bind-mounted into scratch and the env var is
// forwarded unchanged. This is the explicit escape hatch for callers who want
// full control over storage.conf.
//
// Returns the new args slice and a cleanup function that removes any
// temporary file created. The cleanup function is always non-nil and safe to
// defer immediately.
func appendImageStoreArgs(podmanArgs []string, scratch string, opts Options) ([]string, func()) {
	noop := func() {}
	stores := append([]string{}, opts.AdditionalImageStores...)
	// Backward-compatible live-media default: if the SuperISO store is mounted
	// on the host, expose it even when the recipe didn't explicitly pass
	// AdditionalImageStores. This keeps caller-supplied storage.conf files that
	// reference /var/lib/superiso-store working.
	if _, err := os.Stat("/var/lib/superiso-store"); err == nil {
		found := false
		for _, s := range stores {
			if s == "/var/lib/superiso-store" {
				found = true
				break
			}
		}
		if !found {
			stores = append(stores, "/var/lib/superiso-store")
		}
	}
	// Bind-mount each additional store read-only at its host path so any
	// storage.conf entries (caller-supplied or auto-generated) resolve.
	for _, store := range stores {
		podmanArgs = append(podmanArgs, "-v", store+":"+store+":ro")
	}

	// Caller-supplied CONTAINERS_STORAGE_CONF always wins.
	if sc := os.Getenv("CONTAINERS_STORAGE_CONF"); sc != "" {
		podmanArgs = append(podmanArgs,
			"-v", sc+":/etc/containers/storage.conf:ro",
			"-e", "CONTAINERS_STORAGE_CONF=/etc/containers/storage.conf")
		return podmanArgs, noop
	}

	// No caller env override: auto-generate a storage.conf when the recipe
	// declared at least one additional store.
	if len(stores) == 0 {
		return podmanArgs, noop
	}
	hostConf, _, err := writeAdditionalStoresConf(scratch, stores)
	if err != nil {
		progress.Info(fmt.Sprintf("warning: writing additional-stores storage.conf: %v", err))
		return podmanArgs, noop
	}
	podmanArgs = append(podmanArgs,
		"-v", hostConf+":/etc/containers/storage.conf:ro",
		"-e", "CONTAINERS_STORAGE_CONF=/etc/containers/storage.conf")
	cleanup := func() { os.Remove(hostConf) }
	return podmanArgs, cleanup
}

// BootcInstall installs a bootc image to a pre-mounted filesystem.
//
// If opts.SourceImgref is set, bootc is run inside the source container via
// `podman run --privileged`, as required by the bootc documentation.
// If opts.SourceImgref is empty (live-ISO mode), bootc is called directly —
// bootc auto-detects the running container image as the install source.
func BootcInstall(opts Options) error {
	if opts.SourceImgref != "" {
		return bootcViaContainer(opts)
	}
	return bootcDirect(opts)
}

func exportComposefsOCIIfNeeded(opts Options, sourceImgref string) error {
	// Composefs always requires OCI layout for raw blobs.
	// Non-composefs only needs it in container mode with overlay redirect.
	// In direct mode (bootcDirect) for non-composefs, bootc reads from
	// containers-storage on the host — no OCI export needed.
	if !opts.ComposeFsBackend && opts.SourceImgref == "" {
		return nil
	}
	if sourceImgref == "" {
		return fmt.Errorf("OCI layout export requires a source image reference")
	}

	ociDir := filepath.Join(opts.scratchDir(), "oci-cache")
	if err := SkopeoExportOCIFn(sourceImgref, ociDir, opts.scratchDir()); err != nil {
		return fmt.Errorf("exporting image to OCI layout: %w", err)
	}
	return nil
}

// bootcViaContainer runs bootc from inside the source container image.
// This is the required approach when not already running inside the image.
func bootcViaContainer(opts Options) error {
	targetImgref := opts.TargetImgref
	if targetImgref == "" {
		targetImgref = opts.SourceImgref
	}
	// Strip transport prefixes (containers-storage:, docker://, etc.) from
	// the target-imgref so the installed system tracks a clean registry URL
	// for day-2 bootc updates.
	targetImgref = bareImageRef(targetImgref)

	scratch := opts.scratchDir()

	// Disk-back the HOST's /var/tmp for the whole install. bootc's install
	// preflight re-mirrors the container's /var/tmp from the host's (via the
	// host PID namespace), and containers/image hardcodes /var/tmp for its
	// multi-GiB blob staging — neither a storage.conf tmpdir nor a
	// container-side mount survives both (#20/#21). On a live ISO the host
	// /var/tmp is the tiny dracut overlay, so this bind is what makes the
	// mirrored container /var/tmp disk-backed.
	if cleanupVarTmp, vErr := HostVarTmpBindFn(scratch); vErr != nil {
		progress.Info(fmt.Sprintf("warning: could not bind disk-backed dir over host /var/tmp (%v); blob staging may ENOSPC on this host", vErr))
	} else if cleanupVarTmp != nil {
		defer cleanupVarTmp()
	}

	// Non-composefs (ostree/grub2): the default VFS storage driver copies every
	// image layer byte-for-byte when preparing the bootc container, which OOM-kills
	// VMs on large images (>4 GB).  Probe whether the target scratch filesystem
	// supports overlay and redirect podman storage there via --root.  Overlay
	// avoids the copy entirely — working layers are created via mount namespaces —
	// eliminating the memory pressure that kills podman during bootc install.
	var nonComposefsRoot, nonComposefsDriver string
	if !opts.ComposeFsBackend {
		driver, reason := selectStorageDriver(scratch)
		if driver == "overlay" {
			progress.Substep(fmt.Sprintf("Redirecting podman storage to target disk with %s driver (%s)", driver, reason))
			nonComposefsRoot = filepath.Join(scratch, "containers-root")
			nonComposefsDriver = driver
			if err := os.RemoveAll(nonComposefsRoot); err != nil && !os.IsNotExist(err) {
				progress.Substep(fmt.Sprintf("Warning: could not clear previous podman database: %v", err))
			}
			// Only re-pull when the source is a registry URL and we are not on
			// a live ISO.  containers-storage: images are already local; they
			// get exported to OCI layout instead.  Live-ISO hosts (/var on
			// tmpfs/overlayfs) ship the image embedded and must never pull
			// from the registry — the pull would fill the writable overlay.
			if !strings.HasPrefix(opts.SourceImgref, "containers-storage:") && !HostVarConstrainedFn() {
				opts.NeedsPull = true
			}
		} else {
			progress.Substep(fmt.Sprintf("Target-disk overlay unavailable (%s); using host VFS storage", reason))
		}
	}

	// Whether we use OCI layout (instead of containers-storage) for the image
	// source.  Composefs always uses it; non-composefs uses it when redirecting
	// to target disk so we can skip the containers-storage bind mount.
	useOciLayout := opts.ComposeFsBackend || nonComposefsRoot != ""

	if opts.NeedsPull {
		if err := pullImage(opts.SourceImgref, opts.LayerCount, nonComposefsRoot, nonComposefsDriver); err != nil {
			return fmt.Errorf("pulling image: %w", err)
		}
	} else {
		progress.Substep("Image already up to date, skipping pull")
	}

	// Export image to OCI layout when needed.  Composefs requires raw OCI
	// blobs; non-composefs OCI-redirect uses it so we can skip the
	// containers-storage bind mount (the major source of podman memory
	// pressure).
	if useOciLayout {
		if err := exportComposefsOCIIfNeeded(opts, opts.SourceImgref); err != nil {
			return err
		}
	}

	containerOpts := opts
	if useOciLayout {
		containerOpts.ComposeFsOCIPath = containerOCICachePath
	}
	bootcArgs := BuildBootcArgs(containerOpts, targetImgref, "/target")

	// Build the podman run invocation.
	var podmanArgs []string
	podmanImageRef := opts.SourceImgref

	if useOciLayout {
		ociCacheHost := filepath.Join(scratch, "oci-cache")
		podmanImageRef = "oci:" + ociCacheHost

		var containersRoot, storageDriver string
		if opts.ComposeFsBackend {
			containersRoot = filepath.Join(scratch, "containers-root")
			storageDriver, _ = selectStorageDriver(scratch)
		} else {
			containersRoot = nonComposefsRoot
			storageDriver = nonComposefsDriver
		}

		// Clear any previous podman database to avoid driver-mismatch errors;
		// the early RemoveAll when nonComposefsRoot is set handles this for
		// the non-composefs path, so only do it for composefs here.
		if opts.ComposeFsBackend {
			if err := os.RemoveAll(nonComposefsRoot); err != nil && !os.IsNotExist(err) {
				progress.Substep(fmt.Sprintf("Warning: could not clear previous podman database: %v", err))
			}
		}

		progress.Substep(fmt.Sprintf("Using %s storage driver with OCI layout", storageDriver))
		podmanArgs = append(podmanArgs,
			"--root", containersRoot,
			"--storage-driver", storageDriver,
		)
	}

	podmanArgs = append(podmanArgs,
		"run", "--rm",
		"--privileged",
		"--pid=host",
		"--security-opt", "label=disable",
		"-v", "/dev:/dev",
		"-v", "/sys:/sys",
	)

	if _, err := os.Stat("/sys/firmware/efi/efivars"); err == nil {
		podmanArgs = append(podmanArgs, "-v", "/sys/firmware/efi/efivars:/sys/firmware/efi/efivars")
	}

	if useOciLayout {
		ociCacheHost := filepath.Join(scratch, "oci-cache")
		// Belt and braces on top of the whole-install host /var/tmp bind (see
		// HostVarTmpBindFn above): bind the same disk-backed scratch dir at the
		// container's /var/tmp and /tmp and point $TMPDIR at /var/tmp, so any
		// process that writes big files without going through bootc's
		// ensure_mirrored_host_mount still lands on disk, not a tmpfs.
		// The OCI cache is mounted at the dedicated /run/fisherman/oci-cache
		// path below, so bind-mounting the whole scratch dir at /var/tmp does
		// not hide the cache (the original bug in #38).
		podmanArgs = append(podmanArgs, "-v", scratch+":/var/tmp:z")
		podmanArgs = append(podmanArgs, "-v", scratch+":/tmp:z")
		podmanArgs = append(podmanArgs, "-e", "TMPDIR=/var/tmp")
		podmanArgs = append(podmanArgs,
			"-v", ociCacheHost+":"+containerOCICachePath+":ro")
	} else {
		podmanArgs = append(podmanArgs, "-v", scratch+":/var/tmp:z")
		podmanArgs = append(podmanArgs, "-v", scratch+":/tmp:z")
		podmanArgs = append(podmanArgs, "-e", "TMPDIR=/var/tmp")
	}

	podmanArgs = append(podmanArgs,
		"--mount", fmt.Sprintf("type=bind,src=%s,dst=/target,bind-propagation=rslave", opts.Target),
	)

	if NeedsContainerStorageMount(opts) && !useOciLayout {
		podmanArgs = append(podmanArgs, "-v", "/var/lib/containers:/var/lib/containers")
		var cleanupConf func()
		podmanArgs, cleanupConf = appendImageStoreArgs(podmanArgs, scratch, opts)
		defer cleanupConf()
	}

	// When the target system has SELinux disabled and the host has SELinux
	// active, the host's loaded policy may not recognise the image's label
	// types (e.g. AlmaLinux label types on a Fedora host). Inject an
	// LD_PRELOAD shim that silently drops security.selinux xattr writes,
	// preventing EINVAL from the kernel during both bootc's imgstorage
	// setup (lsetfilecon/lsetxattr) and libostree's layer deployment
	// (fsetxattr). Since the target has selinux=disabled, missing per-file
	// labels are harmless.
	if opts.SelinuxDisabled && selinuxActive() {
		shimPath, shimErr := BuildSelinuxBypassShim()
		if shimErr != nil {
			progress.Info(fmt.Sprintf("warning: SELinux bypass shim unavailable (%v); install may fail on cross-policy images", shimErr))
		} else {
			defer os.Remove(shimPath)
			podmanArgs = append(podmanArgs,
				"-v", shimPath+":/fisherman-selinux-bypass.so:z",
				"-e", "LD_PRELOAD=/fisherman-selinux-bypass.so",
			)
		}
	}

	podmanArgs = append(podmanArgs, podmanImageRef)
	podmanArgs = append(podmanArgs, "bootc")
	podmanArgs = append(podmanArgs, bootcArgs...)

	name, args := runner.HostArgs("podman", podmanArgs)
	fmt.Fprintf(os.Stdout, "+ %s %s\n", name, strings.Join(args, " "))

	cmd := exec.Command(name, args...)
	if err := runWithSubsteps(cmd); err != nil {
		return fmt.Errorf("bootc install to-filesystem (via container): %w", err)
	}
	return nil
}

// bootcDirect calls bootc install to-filesystem directly.
// Only valid when fisherman is already running inside the bootc container image
// (i.e. on the live ISO), where bootc auto-detects the source image.
func bootcDirect(opts Options) error {
	// Disk-back the host /var/tmp for the whole install. Even in direct mode
	// bootc and skopeo stage multi-GiB blobs at /var/tmp (hardcoded in
	// containers/image), which on a live ISO is the tiny dracut overlay.
	// bootcDirect is only reached on live-ISO hosts, where /var is
	// space-constrained. See HostVarTmpBindFn.
	if cleanupVarTmp, vErr := HostVarTmpBindFn(opts.scratchDir()); vErr != nil {
		progress.Info(fmt.Sprintf("warning: could not bind disk-backed dir over host /var/tmp (%v); blob staging may ENOSPC on this host", vErr))
	} else if cleanupVarTmp != nil {
		defer cleanupVarTmp()
	}

	// In live-ISO mode bootc still needs the raw OCI layout for composefs
	// installs, but there is no podman-run wrapper path that would export it for
	// us. Use the target ref (falling back to SourceImgref if present) to export
	// the embedded image into the scratch-backed OCI cache before bootc runs.
	sourceImgref := opts.TargetImgref
	if sourceImgref == "" {
		sourceImgref = opts.SourceImgref
	}
	if err := exportComposefsOCIIfNeeded(opts, sourceImgref); err != nil {
		return err
	}

	bargs := BuildBootcArgs(opts, opts.TargetImgref, opts.Target)

	name, args := runner.HostArgs("bootc", bargs)
	fmt.Fprintf(os.Stdout, "+ %s %s\n", name, strings.Join(args, " "))

	cmd := exec.Command(name, args...)
	if err := runWithSubsteps(cmd); err != nil {
		return fmt.Errorf("bootc install to-filesystem: %w", err)
	}
	return nil
}

// BootcToDisk installs a bootc image directly to a block device using
// `bootc install to-disk --via-loopback`. Unlike BootcInstall (to-filesystem),
// this handles partitioning, formatting, OS deployment, and bootloader
// installation entirely inside the container. --via-loopback auto-enables
// --generic-image which skips EFI NVRAM writes and the bootupd presence check,
// required for images like Project Bluefin/Dakota that don't ship bootupd.
//
// diskDevice may be:
//   - A loop device (/dev/loopN): the backing file is found, the loop device
//     is detached, --via-loopback is passed to bootc, then the loop device is
//     re-attached with partscan on return.
//   - A raw image file (any path not starting with /dev/): --via-loopback is
//     passed directly; after install the file is loop-attached and the
//     assigned loop device is returned as effectiveDisk.
//   - A real block device (/dev/sda, /dev/nvme…): installed directly. Note
//     that images without bootupd (e.g. Dakota) will fail until
//     bootc adds first-class systemd-boot support without requiring bootupd.
//
// effectiveDisk is the block device to use for post-install mounts (may differ
// from diskDevice when a raw file is auto-loop-attached after install).
func BootcToDisk(opts Options, diskDevice, filesystem string) (effectiveDisk string, err error) {
	if opts.SourceImgref != "" {
		return bootcToDiskViaContainer(opts, diskDevice, filesystem)
	}
	eff, err := bootcToDiskDirect(opts, diskDevice, filesystem)
	return eff, err
}

func bootcToDiskViaContainer(opts Options, diskDevice, filesystem string) (effectiveDisk string, err error) {
	targetImgref := opts.TargetImgref
	if targetImgref == "" {
		targetImgref = opts.SourceImgref
	}

	if opts.NeedsPull {
		if err := pullImage(opts.SourceImgref, opts.LayerCount, "", ""); err != nil {
			return "", fmt.Errorf("pulling image: %w", err)
		}
	} else {
		progress.Substep("Image already up to date, skipping pull")
	}

	bootcArgs := []string{"install", "to-disk"}
	if targetImgref != "" {
		bootcArgs = append(bootcArgs, "--target-imgref", targetImgref)
	}
	if filesystem != "" {
		bootcArgs = append(bootcArgs, "--filesystem", filesystem)
	}
	if opts.ComposeFsBackend {
		bootcArgs = append(bootcArgs, "--composefs-backend")
	}
	bootcArgs = append(bootcArgs, "--bootloader", "systemd", "--wipe")

	isFile := !strings.HasPrefix(diskDevice, "/dev/")
	isLoop := !isFile && strings.HasPrefix(filepath.Base(diskDevice), "loop")

	scratch := opts.scratchDir()

	// Same whole-install host /var/tmp disk-backing rationale as
	// bootcViaContainer (#20/#21): bootc mirrors the container's /var/tmp from
	// the host's, and containers/image hardcodes /var/tmp for blob staging.
	if cleanupVarTmp, vErr := HostVarTmpBindFn(scratch); vErr != nil {
		progress.Info(fmt.Sprintf("warning: could not bind disk-backed dir over host /var/tmp (%v); blob staging may ENOSPC on this host", vErr))
	} else if cleanupVarTmp != nil {
		defer cleanupVarTmp()
	}

	podmanArgs := []string{
		"run", "--rm",
		"--privileged",
		"--pid=host",
		"--security-opt", "label=disable",
		"-v", "/dev:/dev",
		"-v", "/sys:/sys",
	}

	// Bind-mount EFI variable store for efibootmgr (same rationale as bootcViaContainer).
	if _, err := os.Stat("/sys/firmware/efi/efivars"); err == nil {
		podmanArgs = append(podmanArgs, "-v", "/sys/firmware/efi/efivars:/sys/firmware/efi/efivars")
	}

	// Belts and braces over the whole-install host /var/tmp bind (see the
	// HostVarTmpBindFn call above): bind scratch at the container's /var/tmp and
	// /tmp and point $TMPDIR at /var/tmp so big-file writes outside bootc's
	// ensure_mirrored_host_mount still land on disk, not a tmpfs.
	// The OCI cache is mounted at the dedicated /run/fisherman/oci-cache path,
	// so mounting the whole scratch dir at /var/tmp and /tmp does not hide
	// the cache.
	podmanArgs = append(podmanArgs, "-v", scratch+":/var/tmp:z")
	podmanArgs = append(podmanArgs, "-v", scratch+":/tmp:z")
	podmanArgs = append(podmanArgs, "-e", "TMPDIR=/var/tmp")

	if opts.ComposeFsBackend {
		// composefs-backend requires raw OCI blobs (compressed layer tarballs)
		// that podman pull does not preserve in containers-storage. Export the
		// image to an OCI directory layout via skopeo so the blobs exist on disk,
		// then bind-mount the cache at containerOCICachePath and pass
		// --source-imgref oci:<containerOCICachePath> to bootc.
		ociDir := filepath.Join(scratch, "oci-cache")
		if err := SkopeoExportOCIFn(opts.SourceImgref, ociDir, scratch); err != nil {
			return "", fmt.Errorf("exporting image to OCI layout: %w", err)
		}
		podmanArgs = append(podmanArgs, "-v", ociDir+":"+containerOCICachePath+":ro")
		bootcArgs = append(bootcArgs, "--source-imgref", "oci:"+containerOCICachePath)
		bootcArgs = append(bootcArgs, diskDevice)
		effectiveDisk = diskDevice
	} else {
		// Standard (grub2/ostree) path: bind containers-storage into the container
		// so bootc can read its image layers directly.
		podmanArgs = append(podmanArgs, "-v", "/var/lib/containers:/var/lib/containers")

		// Additional image stores (same rationale as bootcViaContainer).
		var cleanupConf func()
		podmanArgs, cleanupConf = appendImageStoreArgs(podmanArgs, scratch, opts)
		defer cleanupConf()

		// --via-loopback is required for loop devices (BLKRRPART ioctl fails on
		// loop devices so partition nodes never appear inside the container).
		// For regular block devices on the non-composefs path, install directly.
		switch {
		case isFile:
			bootcArgs = append(bootcArgs, "--via-loopback", diskDevice)
		case isLoop:
			backingFile, ferr := loopBackingFile(diskDevice)
			if ferr != nil {
				return "", fmt.Errorf("querying loop backing file: %w", ferr)
			}
			if ferr := loopDetach(diskDevice); ferr != nil {
				return "", fmt.Errorf("detaching loop device: %w", ferr)
			}
			defer func() {
				if rerr := loopReattach(diskDevice, backingFile); rerr != nil {
					fmt.Fprintf(os.Stderr, "warning: re-attaching loop device: %v\n", rerr)
				}
			}()
			bootcArgs = append(bootcArgs, "--via-loopback", backingFile)
			effectiveDisk = diskDevice
		default:
			bootcArgs = append(bootcArgs, diskDevice)
			effectiveDisk = diskDevice
		}

		// For file-based installs, bind-mount the file's directory so the
		// container can access the path passed to --via-loopback.
		if isFile {
			dir := filepath.Dir(diskDevice)
			podmanArgs = append(podmanArgs, "-v", dir+":"+dir)
		}
	}

	podmanArgs = append(podmanArgs, opts.SourceImgref, "bootc")
	podmanArgs = append(podmanArgs, bootcArgs...)

	name, args := runner.HostArgs("podman", podmanArgs)
	fmt.Fprintf(os.Stdout, "+ %s %s\n", name, strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	if err := runWithSubsteps(cmd); err != nil {
		return "", fmt.Errorf("bootc install to-disk (via container): %w", err)
	}

	// For raw file targets, loop-attach the now-installed image so the caller
	// can find and mount partitions.
	if isFile && !opts.ComposeFsBackend {
		loopDev, lerr := loopAttachFile(diskDevice)
		if lerr != nil {
			return "", fmt.Errorf("loop-attaching installed image file: %w", lerr)
		}
		effectiveDisk = loopDev
	}
	return effectiveDisk, nil
}

// DefaultSkopeoExportOCI is the default implementation of SkopeoExportOCIFn.
var DefaultSkopeoExportOCI SkopeoExportFunc = skopeoExportOCI

// SkopeoExportFunc exports an image from containers-storage to an OCI layout
// under destDir, using tmpdir as TMPDIR for skopeo (so multi-gigabyte
// intermediate files land on disk-backed scratch instead of a tmpfs/overlay
// on live ISOs).
type SkopeoExportFunc func(image, destDir, tmpdir string) error

// SkopeoExportOCIFn is the function used by bootcToDiskViaContainer to export
// a composefs image to an OCI layout. Replace in tests to avoid disk I/O.
var SkopeoExportOCIFn SkopeoExportFunc = skopeoExportOCI

// bareImageRef strips any OCI transport prefix from image, returning the bare
// registry reference. This handles both "scheme://ref" (e.g. "docker://") and
// "scheme:ref" (e.g. "containers-storage:") styles. Live-ISO recipes may carry
// a "containers-storage:" prefix on the Image field; the functions below must
// not double-prepend it.
func bareImageRef(image string) string {
	if idx := strings.Index(image, "://"); idx >= 0 {
		return image[idx+3:]
	}
	if idx := strings.Index(image, ":"); idx > 0 {
		if transport := image[:idx]; !strings.ContainsAny(transport, "/.") {
			return image[idx+1:]
		}
	}
	return image
}

// skopeoExportOCI exports an image from containers-storage to an OCI directory
// layout. The composefs-backend requires raw OCI blobs (compressed layer
// tarballs) that podman pull does not preserve; skopeo reconstructs them from
// the tar-split.gz metadata stored alongside the overlay diffs.
func skopeoExportOCI(image, destDir, tmpdir string) error {
	progress.Substep("Exporting image to OCI layout for composefs install")
	// Remove stale export if present.
	if err := os.RemoveAll(destDir); err != nil {
		return fmt.Errorf("removing old OCI cache: %w", err)
	}

	// tmpdir must be disk-backed for multi-gigabyte intermediate files.
	if tmpdir == "" {
		tmpdir = "/var/fisherman-tmp"
	}
	if err := os.MkdirAll(tmpdir, 0o1777); err != nil {
		tmpdir = "/tmp"
	}

	// Redirect /var/tmp to the disk-backed scratch dir before the export.
	//
	// Root cause: containers/image hardcodes /var/tmp for its multi-GiB big-file
	// staging (internal/tmpdir.unixTempDirForBigFiles, only overridable via
	// SystemContext.BigFilesTemporaryDir, which skopeo does not set). On live
	// ISOs /var/tmp is on the dracut overlayfs (~1.4 GiB) — too small for
	// multi-GiB layer blobs. Both podman and skopeo hit this.
	//
	// Fix: bind-mount the scratch dir over /var/tmp in the host mount namespace
	// so the hardcoded path becomes disk-backed. Idempotent with the
	// whole-install binder (HostVarTmpBindFn + hostVarTmpBound) so a
	// standalone export and a container install share one mount without stacking.
	// Deferred umount restores it after export.
	if cleanupVarTmp, vErr := HostVarTmpBindFn(tmpdir); vErr != nil {
		fmt.Fprintf(os.Stdout, "# warning: /var/tmp bind-mount failed (%v) — ENOSPC likely on overlay tmpfs\n", vErr)
	} else if cleanupVarTmp != nil {
		defer cleanupVarTmp()
	}

	skopeoArgs := []string{
		"copy",
		"containers-storage:" + bareImageRef(image),
		"oci:" + destDir,
	}
	name, args := runner.HostArgs("skopeo", skopeoArgs)
	fmt.Fprintf(os.Stdout, "+ %s %s\n", name, strings.Join(args, " "))
	fmt.Fprintf(os.Stdout, "# TMPDIR=%s\n", tmpdir)
	cmd := exec.Command(name, args...)
	if err := runWithSubsteps(cmd); err != nil {
		return fmt.Errorf("skopeo copy: %w", err)
	}
	progress.Substep("OCI export complete")
	return nil
}

// loopBackingFile returns the backing file path for a loop device.
func loopBackingFile(loopDev string) (string, error) {
	name, args := runner.HostArgs("losetup", []string{"--noheadings", "-O", "BACK-FILE", loopDev})
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// loopDetach detaches a loop device.
func loopDetach(loopDev string) error {
	name, args := runner.HostArgs("losetup", []string{"-d", loopDev})
	return exec.Command(name, args...).Run()
}

// loopReattach attaches backingFile to loopDev with --partscan so partition
// nodes are visible on the host.
func loopReattach(loopDev, backingFile string) error {
	name, args := runner.HostArgs("losetup", []string{"-P", loopDev, backingFile})
	return exec.Command(name, args...).Run()
}

// loopAttachFile attaches file to a free loop device with partscan and returns
// the assigned device path (e.g. /dev/loop2).
func loopAttachFile(file string) (string, error) {
	name, args := runner.HostArgs("losetup", []string{"--find", "--partscan", "--show", file})
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func bootcToDiskDirect(opts Options, diskDevice, filesystem string) (string, error) {
	bootcArgs := []string{"install", "to-disk"}
	if opts.TargetImgref != "" {
		bootcArgs = append(bootcArgs, "--target-imgref", opts.TargetImgref)
	}
	if filesystem != "" {
		bootcArgs = append(bootcArgs, "--filesystem", filesystem)
	}
	if opts.ComposeFsBackend {
		bootcArgs = append(bootcArgs, "--composefs-backend")
	}
	bootcArgs = append(bootcArgs,
		"--bootloader", "systemd",
		"--via-loopback",
		"--wipe",
		diskDevice,
	)

	name, args := runner.HostArgs("bootc", bootcArgs)
	fmt.Fprintf(os.Stdout, "+ %s %s\n", name, strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	if err := runWithSubsteps(cmd); err != nil {
		return "", fmt.Errorf("bootc install to-disk: %w", err)
	}
	return diskDevice, nil
}

// Using podman (rather than skopeo copy) ensures the image lands in the same
// storage location and format that bootc will read from inside its container,
// avoiding "file does not exist" blob errors when CONFIG_OVERLAY_FS_REDIRECT_DIR
// is set on the host kernel.
// layerCount is the expected number of layers from CheckImage, used for progress.
func pullImage(image string, layerCount int, root, storageDriver string) error {
	progress.Substep("Pulling container image")
	if layerCount > 0 {
		progress.Substep(fmt.Sprintf("Pulling image: %d layers to download", layerCount))
	}

	podmanArgs := []string{}
	if root != "" {
		podmanArgs = append(podmanArgs, "--root", root)
	}
	if storageDriver != "" {
		podmanArgs = append(podmanArgs, "--storage-driver", storageDriver)
	}
	podmanArgs = append(podmanArgs, "pull", image)
	name, args := runner.HostArgs("podman", podmanArgs)
	fmt.Fprintf(os.Stdout, "+ %s %s\n", name, strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pw.Close()
		return err
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(pr)
		scanner.Buffer(make([]byte, 0, 256*1024), 256*1024)
		layersDone := 0
		for scanner.Scan() {
			line := scanner.Text()
			fmt.Fprintln(os.Stdout, line)
			lower := strings.ToLower(line)

			// podman pull emits "Copying blob sha256:..." lines when not on a TTY.
			if strings.HasPrefix(lower, "copying blob sha256:") {
				layersDone++
				if layerCount > 0 {
					progress.Substep(fmt.Sprintf("Pulling image: layer %d/%d", layersDone, layerCount))
				} else {
					progress.Substep(fmt.Sprintf("Pulling image: layer %d", layersDone))
				}
			} else if strings.Contains(lower, "copying config") {
				progress.Substep("Pulling image: copying config")
			} else if strings.Contains(lower, "writing manifest") {
				progress.Substep("Pulling image: writing manifest")
			}
		}
	}()

	err := cmd.Wait()
	pw.Close()
	<-done

	if err != nil {
		return fmt.Errorf("podman pull %s: %w", image, err)
	}
	progress.Substep("Image pulled successfully")
	return nil
}

// ImageCheck holds the result of a pre-flight image inspection.
type ImageCheck struct {
	NeedsPull  bool // true if the image is absent or stale in containers-storage
	LayerCount int  // number of layers in the remote image; 0 if unknown
	Offline    bool // true if the registry was unreachable but a local copy was found
}

// DefaultSkopeoInspect runs `skopeo inspect <args>` and returns stdout.
func DefaultSkopeoInspect(args ...string) ([]byte, error) {
	name, hargs := runner.HostArgs("skopeo", append([]string{"inspect"}, args...))
	fmt.Fprintf(os.Stdout, "+ %s %s\n", name, strings.Join(hargs, " "))
	return exec.Command(name, hargs...).Output()
}

// SkopeoInspectFn is the function used by CheckImage to call skopeo inspect.
// Replace in tests to avoid network calls.
var SkopeoInspectFn = DefaultSkopeoInspect

// HostVarConstrainedFn reports whether /var lives on a space-constrained
// filesystem (tmpfs/overlayfs), which identifies a live-ISO environment.
// On such hosts the install image is embedded in the ISO and a registry pull
// would fill the tiny writable overlay — so fisherman must use the local copy
// and never pull. Overridable in tests.
var HostVarConstrainedFn = DefaultHostVarConstrained

// DefaultHostVarConstrained is the default implementation of
// HostVarConstrainedFn: a filesystem-type probe of /var. Mirrors the
// isSpaceConstrained("/var") live-environment check in cmd/fisherman.
func DefaultHostVarConstrained() bool {
	ft, err := filesystemType("/var")
	if err != nil {
		return false
	}
	return ft == "tmpfs" || ft == "overlayfs"
}

// CheckImage decides whether a registry pull is required for image.
//
// The local containers-storage copy always wins: the installer's job is to
// deploy the embedded/pre-pulled image, and post-install updates are handled
// by `bootc update`. We only reach for the registry when there is no usable
// local copy AND the registry actually answers — an unreachable registry makes
// a pull attempt doomed, so that is reported as no-pull instead.
//
// If the remote registry is unreachable (offline), CheckImage defers to local
// containers-storage: if the image is present locally it is used as-is
// (NeedsPull=false, Offline=true). This allows a full offline install when the
// image has been pre-pulled into podman storage.
func CheckImage(image string) ImageCheck {
	type manifest struct {
		Digest string   `json:"Digest"`
		Layers []string `json:"Layers"`
	}

	// 1. Local containers-storage copy wins. Never contact the registry when a
	// usable local copy exists.
	localOut, localErr := SkopeoInspectFn("containers-storage:" + bareImageRef(image))
	if localErr == nil {
		var local manifest
		if json.Unmarshal(localOut, &local) == nil {
			return ImageCheck{NeedsPull: false, LayerCount: len(local.Layers)}
		}
	}

	// 2. Live-ISO hosts (/var on tmpfs/overlayfs) ship the image embedded in
	// the ISO. Pulling from the registry would fill the space-constrained
	// writable overlay, so never pull — fail fast on the local copy instead.
	if HostVarConstrainedFn() {
		return ImageCheck{NeedsPull: false, Offline: true}
	}

	// 3. No usable local copy: pull only when the registry answers (resolves
	// fat/multi-arch manifests and doubles as the reachability probe). If it is
	// unreachable the pull is doomed; defer to the local copy, which will
	// surface a clear error if it is truly absent.
	remoteOut, remoteErr := SkopeoInspectFn("docker://" + bareImageRef(image))
	if remoteErr != nil {
		return ImageCheck{NeedsPull: true}
	}
	var remote manifest
	if err := json.Unmarshal(remoteOut, &remote); err != nil {
		return ImageCheck{NeedsPull: true}
	}

	// 3b. Registry reachable and no local copy: pull needed.
	return ImageCheck{NeedsPull: true, LayerCount: len(remote.Layers)}
}

// runWithSubsteps runs a command, relays its combined stdout/stderr line-by-line
// to our stdout, and emits JSON substep events when it recognises bootc progress.
func runWithSubsteps(cmd *exec.Cmd) error {
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pw.Close()
		return err
	}

	// Read lines in a goroutine so we don't block.
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(pr)
		// Increase buffer for long ostree/podman lines.
		scanner.Buffer(make([]byte, 0, 256*1024), 256*1024)
		lastSubstep := ""
		for scanner.Scan() {
			line := scanner.Text()
			// Always relay the raw line to the VTE terminal.
			fmt.Fprintln(os.Stdout, line)
			// Detect bootc / ostree / podman progress keywords and emit substep.
			if sub := ClassifyLine(line); sub != "" && sub != lastSubstep {
				lastSubstep = sub
				progress.Substep(sub)
			}
		}
	}()

	err := cmd.Wait()
	pw.Close()
	<-done
	return err
}

// ClassifyLine maps a raw bootc/ostree/podman output line to a human-readable
// substep description, or "" if the line is not interesting.
func ClassifyLine(line string) string {
	lower := strings.ToLower(line)
	switch {
	case strings.Contains(lower, "installing image:"):
		return "Deploying image"
	case strings.Contains(lower, "layers") && strings.Contains(lower, "needed"):
		// e.g. "layers already present: 0; layers needed: 64 (3.7 GB)"
		// This line fires right before a long silent deployment phase — warn the user.
		if i := strings.Index(lower, "layers needed:"); i >= 0 {
			rest := strings.TrimSpace(line[i+len("layers needed:"):])
			return "Writing " + rest + " to disk — this may take several minutes"
		}
		return "Writing image layers to disk — this may take several minutes"
	case strings.Contains(lower, "initializing ostree"):
		return "Initializing ostree layout"
	case strings.Contains(lower, "deploying container image"):
		// This line is buffered by bootc and only flushed once deployment is fully
		// done ("Deploying container image...done (N minutes)"). It fires AFTER the
		// long wait, so the message should reflect that the hard work is over.
		return "OS deployed, installing bootloader"
	case strings.Contains(lower, "bootloader:"):
		return "Detected bootloader"
	case strings.Contains(lower, "installing bootloader"):
		return "Installing bootloader"
	case strings.Contains(lower, "efibootmgr"):
		return "Configuring EFI boot entry"
	case strings.Contains(lower, "installed:") && strings.Contains(lower, "grub"):
		return "Configuring GRUB"
	case strings.Contains(lower, "installation complete"):
		return "bootc installation complete"
	case strings.Contains(lower, "selinux"):
		return "Configuring SELinux"
	case strings.Contains(lower, "generating initramfs") || strings.Contains(lower, "dracut"):
		return "Generating initramfs"
	}
	return ""
}
