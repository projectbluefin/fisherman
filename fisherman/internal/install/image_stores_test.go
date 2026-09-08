package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAppendImageStoreArgs_NoStoresNoEnv is the trivial case: no caller env,
// no recipe-supplied stores → no podman flags appended, no temp file created.
func TestAppendImageStoreArgs_NoStoresNoEnv(t *testing.T) {
	t.Setenv("CONTAINERS_STORAGE_CONF", "")
	scratch := t.TempDir()
	out, cleanup := appendImageStoreArgs(nil, scratch, Options{})
	defer cleanup()
	if len(out) != 0 {
		t.Errorf("expected no args; got %v", out)
	}
	// No file should have been created under scratch/fisherman-conf.
	if entries, _ := os.ReadDir(scratch + "/fisherman-conf"); len(entries) != 0 {
		t.Errorf("expected no conf file; got %d entries", len(entries))
	}
}

// TestAppendImageStoreArgs_WritesGeneratedConf verifies that when the recipe
// declares additional stores (and no caller env override is set), each store
// is bind-mounted read-only and a generated storage.conf listing them all is
// passed via CONTAINERS_STORAGE_CONF.
func TestAppendImageStoreArgs_WritesGeneratedConf(t *testing.T) {
	t.Setenv("CONTAINERS_STORAGE_CONF", "")
	scratch := t.TempDir()
	opts := Options{AdditionalImageStores: []string{
		"/var/lib/store-a",
		"/var/lib/store-b",
	}}
	out, cleanup := appendImageStoreArgs(nil, scratch, opts)
	defer cleanup()

	joined := strings.Join(out, " ")
	for _, want := range []string{
		"/var/lib/store-a:/var/lib/store-a:ro",
		"/var/lib/store-b:/var/lib/store-b:ro",
		"CONTAINERS_STORAGE_CONF=/etc/containers/storage.conf",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("podman args missing %q; got: %v", want, out)
		}
	}

	// Locate the generated host-side conf file and assert it lists both stores.
	confDir := scratch + "/fisherman-conf"
	entries, err := os.ReadDir(confDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected exactly one generated conf in %s; got %v err=%v",
			confDir, entries, err)
	}
	body, err := os.ReadFile(confDir + "/" + entries[0].Name())
	if err != nil {
		t.Fatalf("reading generated conf: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, `"/var/lib/store-a"`) || !strings.Contains(s, `"/var/lib/store-b"`) {
		t.Errorf("generated conf missing stores; body=%q", s)
	}
	if !strings.Contains(s, "additionalimagestores = [") {
		t.Errorf("generated conf missing additionalimagestores key; body=%q", s)
	}
}

// TestAppendImageStoreArgs_CallerEnvWins verifies the documented escape hatch:
// when CONTAINERS_STORAGE_CONF is set, fisherman bind-mounts it into scratch
// and forwards the env var as-is. No fisherman-generated conf is written.
func TestAppendImageStoreArgs_CallerEnvWins(t *testing.T) {
	scratch := t.TempDir()
	callerConf := scratch + "/caller-storage.conf"
	if err := os.WriteFile(callerConf, []byte("# caller\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINERS_STORAGE_CONF", callerConf)

	opts := Options{AdditionalImageStores: []string{"/var/lib/store-x"}}
	out, cleanup := appendImageStoreArgs(nil, scratch, opts)
	defer cleanup()

	joined := strings.Join(out, " ")
	// Store still gets bind-mounted (so storage.conf entries resolve)…
	if !strings.Contains(joined, "/var/lib/store-x:/var/lib/store-x:ro") {
		t.Errorf("expected store bind-mount; got: %v", out)
	}
	// …but the caller's conf wins for the env var.
	if !strings.Contains(joined, "CONTAINERS_STORAGE_CONF=/etc/containers/storage.conf") {
		t.Errorf("expected caller env to win; got: %v", out)
	}
	// And no auto-generated file should have been written under fisherman-conf.
	if entries, _ := os.ReadDir(scratch + "/fisherman-conf"); len(entries) != 0 {
		t.Errorf("auto-generated conf should be skipped when caller env wins; got %d entries", len(entries))
	}
}

// TestVarTmpOverrideHelpers sanity-checks the host /var/tmp override plumbing:
// the override directory path derivation and the idempotence probe that stops
// the whole-install binder and skopeoExportOCI from stacking bind mounts.
func TestVarTmpOverrideHelpers(t *testing.T) {
	scratch := t.TempDir()
	override := varTmpOverrideDir(scratch)
	if override != filepath.Join(scratch, "var-tmp-override") {
		t.Errorf("varTmpOverrideDir = %q, want %q", override, filepath.Join(scratch, "var-tmp-override"))
	}
	if err := os.MkdirAll(override, 0o1777); err != nil {
		t.Fatal(err)
	}
	// /var/tmp is real dir on a real filesystem, override is a fresh dir on
	// another (t.TempDir) filesystem → must not be reported as bound.
	if hostVarTmpBound(override) {
		t.Error("hostVarTmpBound(true) before any bind; is /var/tmp really an alias of the override dir?")
	}
}

func TestDefaultHostVarConstrained(t *testing.T) {
	// DefaultHostVarConstrained inspects /var's filesystem type.
	// If /var is tmpfs or overlayfs, it should return true; otherwise false.
	ft, err := filesystemType("/var")
	got := DefaultHostVarConstrained()
	if err != nil {
		if got != false {
			t.Errorf("DefaultHostVarConstrained() = %v when filesystemType errors, want false", got)
		}
	} else if ft == "tmpfs" || ft == "overlayfs" {
		if !got {
			t.Errorf("DefaultHostVarConstrained() = false, want true for /var on %s", ft)
		}
	} else {
		if got {
			t.Errorf("DefaultHostVarConstrained() = true, want false for /var on %s", ft)
		}
	}
}

func TestDefaultHostVarTmpBind(t *testing.T) {
	scratch := t.TempDir()
	override := varTmpOverrideDir(scratch)

	// In non-root test environments (e.g. unprivileged CI or local dev),
	// mount will fail with EPERM. In privileged CI, it may succeed.
	// Either way, DefaultHostVarTmpBind must ensure the directory is created (01777).
	cleanup, err := DefaultHostVarTmpBind(scratch)
	info, statErr := os.Stat(override)
	if statErr != nil {
		t.Fatalf("override dir %s not created: %v", override, statErr)
	}
	if !info.IsDir() {
		t.Fatalf("override path %s is not a directory", override)
	}

	if err == nil && cleanup != nil {
		// If mount succeeded (running as root / privileged), test idempotency and cleanup unmount.
		cleanup2, err2 := DefaultHostVarTmpBind(scratch)
		if err2 != nil {
			t.Errorf("second DefaultHostVarTmpBind call failed: %v", err2)
		}
		if cleanup2 != nil {
			cleanup2()
		}
		cleanup()
	}
}
