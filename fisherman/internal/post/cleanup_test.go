package post_test

import (
	"io"
	"testing"

	"github.com/tuna-os/fisherman/internal/post"
	"github.com/tuna-os/fisherman/internal/runner"
)

type execCall struct {
	name string
	args []string
}

type recorder struct {
	calls []execCall
	err   error
}

func (r *recorder) run(stdin io.Reader, name string, args ...string) error {
	r.calls = append(r.calls, execCall{name: name, args: args})
	return r.err
}

func setupRecorder(t *testing.T) *recorder {
	t.Helper()
	rec := &recorder{}
	runner.RunFn = rec.run
	t.Cleanup(func() { runner.RunFn = runner.DefaultRun })
	return rec
}

func setupRemoveAllRecorder(t *testing.T, rec *recorder) {
	t.Helper()
	old := post.RemoveAllFn
	post.RemoveAllFn = func(path string) error {
		rec.calls = append(rec.calls, execCall{name: "removeAll", args: []string{path}})
		return nil
	}
	t.Cleanup(func() { post.RemoveAllFn = old })
}

// TestCleanup_Empty verifies that Cleanup.Run() with no mounts or LUKS is a no-op.
func TestCleanup_Empty(t *testing.T) {
	rec := setupRecorder(t)

	var c post.Cleanup
	c.Run()

	if len(rec.calls) != 0 {
		t.Errorf("expected no calls for empty Cleanup, got %d: %v", len(rec.calls), rec.calls)
	}
}

// TestCleanup_MountsInReverseOrder verifies that mounts are unmounted in reverse
// registration order (LIFO), matching the mount hierarchy (deepest first).
func TestCleanup_MountsInReverseOrder(t *testing.T) {
	rec := setupRecorder(t)

	var c post.Cleanup
	c.AddMount("/mnt/target")
	c.AddMount("/mnt/target/boot")
	c.AddMount("/mnt/target/boot/efi")

	c.Run()

	// Filter to umount calls only.
	var umounts []execCall
	for _, call := range rec.calls {
		if call.name == "umount" {
			umounts = append(umounts, call)
		}
	}

	if len(umounts) != 3 {
		t.Fatalf("expected 3 umount calls, got %d: %v", len(umounts), rec.calls)
	}

	// Reverse order: efi first, then boot, then target.
	wantOrder := []string{
		"/mnt/target/boot/efi",
		"/mnt/target/boot",
		"/mnt/target",
	}
	for i, want := range wantOrder {
		got := umounts[i].args[len(umounts[i].args)-1] // last arg is the mount point
		if got != want {
			t.Errorf("umount[%d] = %q, want %q (must be reverse-order)", i, got, want)
		}
	}
}

// TestCleanup_UmountArgs verifies the exact arguments passed to umount.
func TestCleanup_UmountArgs(t *testing.T) {
	rec := setupRecorder(t)

	var c post.Cleanup
	c.AddMount("/mnt/target")
	c.Run()

	if len(rec.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(rec.calls))
	}
	call := rec.calls[0]
	if call.name != "umount" {
		t.Errorf("name = %q, want umount", call.name)
	}
	// Cleanup uses -R (recursive) to handle bind mounts and submounts.
	if len(call.args) < 2 || call.args[0] != "-R" {
		t.Errorf("args = %v, want [-R <path>]", call.args)
	}
}

// TestCleanup_Idempotent verifies that calling Run() twice only unmounts once.
func TestCleanup_Idempotent(t *testing.T) {
	rec := setupRecorder(t)

	var c post.Cleanup
	c.AddMount("/mnt/target")

	c.Run() // first call — should emit umount
	firstCount := len(rec.calls)

	c.Run() // second call — must be a no-op

	if len(rec.calls) != firstCount {
		t.Errorf("second Run() emitted %d additional calls (must be idempotent)",
			len(rec.calls)-firstCount)
	}
}

// TestCleanup_WithLUKS verifies that a registered LUKS mapper is closed after
// all mounts are unmounted, with proper I/O flushing and reference release.
func TestCleanup_WithLUKS(t *testing.T) {
	rec := setupRecorder(t)

	var c post.Cleanup
	c.AddMount("/mnt/target")
	c.SetLUKS("fisherman-root")

	c.Run()

	// Expect: umount /mnt/target, fuser -km, blockdev --flushbufs, udevadm settle,
	// then cryptsetup luksClose fisherman-root.
	if len(rec.calls) < 5 {
		t.Fatalf("expected at least 5 calls, got %d: %v", len(rec.calls), rec.calls)
	}

	umount := rec.calls[0]
	if umount.name != "umount" {
		t.Errorf("first call: name = %q, want umount", umount.name)
	}

	// Verify cleanup sequence before luksClose
	callNames := make([]string, len(rec.calls))
	for i, call := range rec.calls {
		callNames[i] = call.name
	}

	// Should have fuser, blockdev, udevadm before cryptsetup
	var fuserIdx, blockdevIdx, udevadmIdx, cryptsetupIdx int
	for i, name := range callNames {
		if name == "fuser" {
			fuserIdx = i
		}
		if name == "blockdev" {
			blockdevIdx = i
		}
		if name == "udevadm" {
			udevadmIdx = i
		}
		if name == "cryptsetup" {
			cryptsetupIdx = i
		}
	}

	if fuserIdx == 0 || blockdevIdx == 0 || udevadmIdx == 0 || cryptsetupIdx == 0 {
		t.Errorf("missing cleanup calls: fuser=%d blockdev=%d udevadm=%d cryptsetup=%d",
			fuserIdx, blockdevIdx, udevadmIdx, cryptsetupIdx)
	}
	if fuserIdx > cryptsetupIdx || blockdevIdx > cryptsetupIdx || udevadmIdx > cryptsetupIdx {
		t.Errorf("cleanup sequence out of order: fuser=%d blockdev=%d udevadm=%d cryptsetup=%d (cryptsetup must be last)",
			fuserIdx, blockdevIdx, udevadmIdx, cryptsetupIdx)
	}

	luksClose := rec.calls[cryptsetupIdx]
	if len(luksClose.args) < 2 || luksClose.args[0] != "luksClose" {
		t.Errorf("cryptsetup call: args = %v, want [luksClose fisherman-root]", luksClose.args)
	}
	if luksClose.args[1] != "fisherman-root" {
		t.Errorf("luksClose mapper = %q, want fisherman-root", luksClose.args[1])
	}
}

// TestCleanup_PostRemovalsHappenAfterUnmountsAndLUKS verifies the new
// post-removal contract: paths registered via AddPostRemoval must be deleted
// *after* every unmount and after the LUKS device is closed, never interleaved
// with them. This is the safety property fisherman relies on so that the
// scratch directory (which is itself bind-mounted at /var/tmp inside the bootc
// container and may host the OCI cache) stays accessible until the very end
// of teardown — including the fatal()/os.Exit(1) error path.
func TestCleanup_PostRemovalsHappenAfterUnmountsAndLUKS(t *testing.T) {
	rec := setupRecorder(t)
	setupRemoveAllRecorder(t, rec)

	var c post.Cleanup
	c.AddMount("/mnt/target")
	c.AddMount("/mnt/target/.fisherman-scratch")
	c.SetLUKS("fisherman-root")
	c.AddPostRemoval("/mnt/target/.fisherman-scratch")

	c.Run()

	// Locate the first removeAll and the last umount / cryptsetup luksClose.
	lastTeardownIdx := -1
	firstRemoveIdx := -1
	for i, call := range rec.calls {
		switch call.name {
		case "umount", "cryptsetup", "fuser", "blockdev", "udevadm":
			if i > lastTeardownIdx {
				lastTeardownIdx = i
			}
		case "removeAll":
			if firstRemoveIdx == -1 {
				firstRemoveIdx = i
			}
		}
	}
	if firstRemoveIdx == -1 {
		t.Fatalf("no removeAll call recorded; got calls: %v", rec.calls)
	}
	if firstRemoveIdx < lastTeardownIdx {
		t.Errorf("post-removal at idx %d ran before final teardown call at idx %d: %v",
			firstRemoveIdx, lastTeardownIdx, rec.calls)
	}
	if rec.calls[firstRemoveIdx].args[0] != "/mnt/target/.fisherman-scratch" {
		t.Errorf("removeAll target = %q, want /mnt/target/.fisherman-scratch",
			rec.calls[firstRemoveIdx].args[0])
	}
}

// TestCleanup_NoPostRemovalWhenNotRegistered ensures we don't blindly remove
// the mount paths themselves — only paths explicitly registered.
func TestCleanup_NoPostRemovalWhenNotRegistered(t *testing.T) {
	rec := setupRecorder(t)
	setupRemoveAllRecorder(t, rec)

	var c post.Cleanup
	c.AddMount("/mnt/target")
	c.Run()

	for _, call := range rec.calls {
		if call.name == "removeAll" {
			t.Errorf("unexpected removeAll call: %v", call)
		}
	}
}

// TestCleanup_NoLUKS verifies that no cryptsetup call is made when SetLUKS was
// not called.
func TestCleanup_NoLUKS(t *testing.T) {
	rec := setupRecorder(t)

	var c post.Cleanup
	c.AddMount("/mnt/target")
	c.Run()

	for _, call := range rec.calls {
		if call.name == "cryptsetup" {
			t.Errorf("unexpected cryptsetup call when no LUKS mapper registered: %v", call)
		}
	}
}

// TestCleanup_ReleaseScratchFreesSpaceEarly is the regression gate for the
// "error writing hostname" install failure: the multi-GB OCI scratch cache
// lives on the *target* disk during `bootc install`, and if it is only removed
// by Cleanup.Run() at the very end, every post-install write (flatpak copy,
// hostname, fstab) competes with it for space and dies with ENOSPC.
// ReleaseScratch must unmount and delete it immediately.
func TestCleanup_ReleaseScratchFreesSpaceEarly(t *testing.T) {
	rec := setupRecorder(t)
	setupRemoveAllRecorder(t, rec)

	const scratch = "/mnt/target/.fisherman-scratch"

	var c post.Cleanup
	c.AddMount("/mnt/target")
	c.AddMount(scratch)
	c.AddPostRemoval(scratch)

	if err := c.ReleaseScratch(scratch); err != nil {
		t.Fatalf("ReleaseScratch: %v", err)
	}

	if len(rec.calls) != 2 {
		t.Fatalf("expected umount + removeAll, got %v", rec.calls)
	}
	if rec.calls[0].name != "umount" || rec.calls[0].args[1] != scratch {
		t.Errorf("first call = %v, want umount -R %s", rec.calls[0], scratch)
	}
	if rec.calls[1].name != "removeAll" || rec.calls[1].args[0] != scratch {
		t.Errorf("second call = %v, want removeAll %s", rec.calls[1], scratch)
	}

	// The scratch path must be deregistered so final teardown neither
	// re-unmounts it (spurious "not mounted" warning) nor re-deletes it.
	rec.calls = nil
	c.Run()
	for _, call := range rec.calls {
		for _, arg := range call.args {
			if arg == scratch {
				t.Errorf("Run() still touched released scratch path: %v", rec.calls)
			}
		}
	}
	if len(rec.calls) != 1 || rec.calls[0].args[1] != "/mnt/target" {
		t.Errorf("Run() calls = %v, want a single umount of /mnt/target", rec.calls)
	}
}

// TestCleanup_ReleaseScratchWithoutMount covers the non-live path, where the
// scratch dir is a plain directory on the host and was never bind-mounted.
func TestCleanup_ReleaseScratchWithoutMount(t *testing.T) {
	rec := setupRecorder(t)
	setupRemoveAllRecorder(t, rec)

	var c post.Cleanup
	c.AddPostRemoval("/var/fisherman-tmp")

	if err := c.ReleaseScratch("/var/fisherman-tmp"); err != nil {
		t.Fatalf("ReleaseScratch: %v", err)
	}

	if len(rec.calls) != 1 || rec.calls[0].name != "removeAll" {
		t.Fatalf("expected removeAll only (no umount), got %v", rec.calls)
	}
}
