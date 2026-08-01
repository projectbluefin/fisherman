package post_test

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/tuna-os/fisherman/internal/post"
	"github.com/tuna-os/fisherman/internal/progress"
	"github.com/tuna-os/fisherman/internal/runner"
)

// mockCommand implements runner.Command for testing.
type mockCommand struct {
	name   string
	args   []string
	stdout io.Writer
	stderr io.Writer
	stdin  io.Reader
	output []byte
	err    error
}

func (c *mockCommand) Run() error   { return c.err }
func (c *mockCommand) Start() error { return c.err }
func (c *mockCommand) Wait() error  { return c.err }
func (c *mockCommand) Output() ([]byte, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.output, nil
}
func (c *mockCommand) SetStdin(r io.Reader)  { c.stdin = r }
func (c *mockCommand) SetStdout(w io.Writer) { c.stdout = w }
func (c *mockCommand) SetStderr(w io.Writer) { c.stderr = w }

// mockExecutor implements runner.Executor for testing.
type mockExecutor struct {
	calls []execCall
	// responses maps a command (joined by space) to a response output/error.
	responses map[string]struct {
		out []byte
		err error
	}
}

func (e *mockExecutor) Command(name string, args ...string) runner.Command {
	e.calls = append(e.calls, execCall{name: name, args: args})
	full := name
	if len(args) > 0 {
		full += " " + strings.Join(args, " ")
	}

	// Try exact match first, then prefix match for things like 'du -sb /var/lib/flatpak'
	res, ok := e.responses[full]
	if !ok {
		// Fallback for du/flatpak list calls which might have dynamic paths in tests.
		for k, v := range e.responses {
			if strings.HasPrefix(full, k) {
				res = v
				break
			}
		}
	}

	return &mockCommand{
		name:   name,
		args:   args,
		output: res.out,
		err:    res.err,
	}
}

func setupMockExec(t *testing.T) *mockExecutor {
	t.Helper()
	mock := &mockExecutor{
		responses: make(map[string]struct {
			out []byte
			err error
		}),
	}
	old := post.Exec
	post.Exec = mock
	t.Cleanup(func() { post.Exec = old })
	return mock
}

// TestWriteHostname_ComposeFsNative verifies that for a composefs-native
// deployment hostname is written to the deploy etc dir returned by
// ComposeFsDeployEtcDirFn, not to $TARGET/etc/hostname directly.
func TestWriteHostname_ComposeFsNative(t *testing.T) {
	target := t.TempDir()

	// Set up the stub deploy etc dir (simulates state/deploy/<hash>/etc).
	deployEtc := filepath.Join(target, "state", "deploy", "abc123", "etc")
	if err := os.MkdirAll(deployEtc, 0o755); err != nil {
		t.Fatal(err)
	}

	// Override ComposeFsDeployEtcDirFn to return our stub path.
	old := post.ComposeFsDeployEtcDirFn
	post.ComposeFsDeployEtcDirFn = func(string) (string, error) { return deployEtc, nil }
	t.Cleanup(func() { post.ComposeFsDeployEtcDirFn = old })

	if err := post.WriteHostname(target, "myhost"); err != nil {
		t.Fatalf("WriteHostname: %v", err)
	}

	// Hostname must land in the deploy etc, not in $TARGET/etc.
	hostnameFile := filepath.Join(deployEtc, "hostname")
	data, err := os.ReadFile(hostnameFile)
	if err != nil {
		t.Fatalf("reading hostname file from deploy etc: %v", err)
	}
	if string(data) != "myhost\n" {
		t.Errorf("hostname file content = %q, want %q", string(data), "myhost\n")
	}
	// Confirm nothing was written to the wrong path.
	if _, err := os.Stat(filepath.Join(target, "etc", "hostname")); err == nil {
		t.Error("hostname was unexpectedly written to $TARGET/etc/hostname (wrong path)")
	}
}

// TestWriteHostname_OstreeBackend verifies that when /ostree/ exists under the
// target (ostree-based deployment), hostname is written to the path returned by
// DeploymentDirFn, not to $TARGET/etc/hostname directly.
func TestWriteHostname_OstreeBackend(t *testing.T) {
	target := t.TempDir()
	if err := os.MkdirAll(filepath.Join(target, "ostree"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeDeployDir := filepath.Join(target, "ostree", "deploy", "default", "deploy", "abc123.0")
	if err := os.MkdirAll(fakeDeployDir, 0o755); err != nil {
		t.Fatal(err)
	}

	post.DeploymentDirFn = func(sysroot string) (string, error) {
		return fakeDeployDir, nil
	}
	t.Cleanup(func() { post.DeploymentDirFn = post.DefaultDeploymentDir })

	if err := post.WriteHostname(target, "tunahost"); err != nil {
		t.Fatalf("WriteHostname: %v", err)
	}

	hostnameInDeploy := filepath.Join(fakeDeployDir, "etc", "hostname")
	data, err := os.ReadFile(hostnameInDeploy)
	if err != nil {
		t.Fatalf("reading hostname from deploy dir: %v", err)
	}
	if string(data) != "tunahost\n" {
		t.Errorf("hostname = %q, want %q", string(data), "tunahost\n")
	}
}

// TestCopyFlatpaks_NoLocalData verifies that when no local flatpak data is found,
// the function returns successfully without attempting a copy.
func TestCopyFlatpaks_NoLocalData(t *testing.T) {
	mock := setupMockExec(t)
	target := t.TempDir()

	// Mock 'du -sb /var/lib/flatpak' returning 0
	mock.responses["du -sb /var/lib/flatpak"] = struct {
		out []byte
		err error
	}{out: []byte("0\t/var/lib/flatpak\n")}

	if err := post.CopyFlatpaks(target, nil, ""); err != nil {
		t.Fatalf("CopyFlatpaks: %v", err)
	}

	// Verify no tar or flatpak install calls were made
	for _, call := range mock.calls {
		if call.name == "tar" || (call.name == "flatpak" && len(call.args) > 0 && call.args[0] == "install") {
			t.Errorf("unexpected command call: %s %v", call.name, call.args)
		}
	}
}

// TestCopyFlatpaks_PromotesUserApps verifies that wanted apps missing from the
// system installation are promoted from the user installation.
func TestCopyFlatpaks_PromotesUserApps(t *testing.T) {
	mock := setupMockExec(t)
	target := t.TempDir()

	// 1. Mock 'flatpak list --system --app' (empty)
	mock.responses["flatpak list --system --columns=ref --app"] = struct {
		out []byte
		err error
	}{out: []byte("")}

	// 2. Mock 'flatpak list --system' (empty)
	mock.responses["flatpak list --system --columns=ref"] = struct {
		out []byte
		err error
	}{out: []byte("")}

	// 3. Mock 'flatpak list --user --columns=ref --app' (contains wanted app)
	wanted := "org.mozilla.firefox"
	mock.responses["flatpak list --user --columns=ref --app"] = struct {
		out []byte
		err error
	}{out: []byte(wanted + "/x86_64/stable\n")}

	// 4. Mock 'du -sb /var/lib/flatpak' (has data)
	mock.responses["du -sb /var/lib/flatpak"] = struct {
		out []byte
		err error
	}{out: []byte("1024\t/var/lib/flatpak\n")}

	// 5. Mock 'flatpak install --system' (success)
	mock.responses[fmt.Sprintf("flatpak install --system -y --noninteractive %s/x86_64/stable", wanted)] = struct {
		out []byte
		err error
	}{out: []byte("OK")}

	if err := post.CopyFlatpaks(target, []string{wanted}, ""); err != nil {
		t.Fatalf("CopyFlatpaks: %v", err)
	}

	// Verify promotion call
	promoted := false
	for _, call := range mock.calls {
		if call.name == "flatpak" && len(call.args) > 0 && call.args[0] == "install" {
			for _, arg := range call.args {
				if strings.Contains(arg, wanted) {
					promoted = true
				}
			}
		}
	}
	if !promoted {
		t.Errorf("expected %s to be promoted to system, but no flatpak install call found", wanted)
	}
}

// TestCopyFlatpaks_EmitsPerAppSubsteps verifies that a substep is emitted for
// each wanted app that is found in the system install, so the UI can show
// individual app names as they are copied.
func TestCopyFlatpaks_EmitsPerAppSubsteps(t *testing.T) {
	mock := setupMockExec(t)
	target := t.TempDir()

	apps := []string{"org.mozilla.firefox", "org.gnome.Console"}
	refs := apps[0] + "/x86_64/stable\n" + apps[1] + "/x86_64/stable\n"

	mock.responses["flatpak list --system --columns=ref --app"] = struct {
		out []byte
		err error
	}{out: []byte(refs)}
	mock.responses["flatpak list --system --columns=ref"] = struct {
		out []byte
		err error
	}{out: []byte(refs)}
	mock.responses["flatpak list --user --columns=ref --app"] = struct {
		out []byte
		err error
	}{out: []byte("")}
	mock.responses["du -sb /var/lib/flatpak"] = struct {
		out []byte
		err error
	}{out: []byte("2048\t/var/lib/flatpak\n")}

	var substeps []string
	origSubstep := progress.SubstepFn
	progress.SubstepFn = func(msg string) { substeps = append(substeps, msg) }
	defer func() { progress.SubstepFn = origSubstep }()

	if err := post.CopyFlatpaks(target, apps, ""); err != nil {
		t.Fatalf("CopyFlatpaks: %v", err)
	}

	// Both wanted app names must appear in substep messages.
	for _, app := range apps {
		found := false
		for _, s := range substeps {
			if strings.Contains(s, app) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no substep message contained app name %q; substeps: %v", app, substeps)
		}
	}
}

// TestCopyFlatpaks_FlatpakVarPathOverride verifies that when flatpakVarPath is
// set (e.g. for GnomeOS/Dakota "state/os/default/var"), CopyFlatpaks writes
// to target/<flatpakVarPath>/lib/flatpak rather than the composefs default.
func TestCopyFlatpaks_FlatpakVarPathOverride(t *testing.T) {
	mock := setupMockExec(t)
	target := t.TempDir()

	// Simulate no local flatpak data (nothing to copy, just verify the mkdir target).
	mock.responses["du -sb /var/lib/flatpak"] = struct {
		out []byte
		err error
	}{out: []byte("0\t/var/lib/flatpak\n")}

	flatpakVarPath := "state/os/default/var"
	if err := post.CopyFlatpaks(target, nil, flatpakVarPath); err != nil {
		t.Fatalf("CopyFlatpaks: %v", err)
	}

	want := filepath.Join(target, flatpakVarPath, "lib", "flatpak")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("expected flatpak dir at %s to exist, got: %v", want, err)
	}

	// Ensure the legacy top-level var/lib/flatpak was NOT created.
	legacy := filepath.Join(target, "var", "lib", "flatpak")
	if _, err := os.Stat(legacy); err == nil {
		t.Errorf("legacy path %s was created — flatpakVarPath override was ignored", legacy)
	}
}

func TestEnsurePlymouthArgs(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantOut string
		wantMod bool
	}{
		{
			name:    "adds rhgb and quiet when absent",
			input:   "title Fedora Linux\noptions root=UUID=abc rw\n",
			wantOut: "title Fedora Linux\noptions root=UUID=abc rw rhgb quiet\n",
			wantMod: true,
		},
		{
			name:    "adds only missing arg",
			input:   "options root=UUID=abc rw rhgb\n",
			wantOut: "options root=UUID=abc rw rhgb quiet\n",
			wantMod: true,
		},
		{
			name:    "no change when args already present",
			input:   "options root=UUID=abc rw rhgb quiet\n",
			wantOut: "options root=UUID=abc rw rhgb quiet\n",
			wantMod: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			entriesDir := dir + "/boot/loader/entries"
			if err := os.MkdirAll(entriesDir, 0o755); err != nil {
				t.Fatal(err)
			}
			entryPath := entriesDir + "/test.conf"
			if err := os.WriteFile(entryPath, []byte(tc.input), 0o644); err != nil {
				t.Fatal(err)
			}
			n, err := post.EnsurePlymouthArgs(dir)
			if err != nil {
				t.Fatalf("EnsurePlymouthArgs: %v", err)
			}
			if tc.wantMod && n == 0 {
				t.Error("expected entry to be modified, but it was not")
			}
			if !tc.wantMod && n != 0 {
				t.Error("expected no modification, but entry was changed")
			}
			got, _ := os.ReadFile(entryPath)
			if string(got) != tc.wantOut {
				t.Errorf("entry content:\ngot:  %q\nwant: %q", string(got), tc.wantOut)
			}
		})
	}
}

func TestEnsureLuksArgs(t *testing.T) {
	const testUUID = "1520bba9-010e-443d-b082-2fe56abdfee1"
	const wantArg = "rd.luks.name=" + testUUID + "=root"

	t.Run("injects rd.luks.name into grub path", func(t *testing.T) {
		dir := t.TempDir()
		entriesDir := dir + "/boot/loader/entries"
		if err := os.MkdirAll(entriesDir, 0o755); err != nil {
			t.Fatal(err)
		}
		entryPath := entriesDir + "/test.conf"
		input := "title Fedora Linux\noptions root=UUID=abc rw\n"
		if err := os.WriteFile(entryPath, []byte(input), 0o644); err != nil {
			t.Fatal(err)
		}
		n, err := post.EnsureLuksArgs(dir, testUUID)
		if err != nil {
			t.Fatalf("EnsureLuksArgs: %v", err)
		}
		if n != 1 {
			t.Errorf("expected 1 entry modified, got %d", n)
		}
		got, _ := os.ReadFile(entryPath)
		if !strings.Contains(string(got), wantArg) {
			t.Errorf("entry missing %q:\n%s", wantArg, got)
		}
	})

	t.Run("injects rd.luks.name into systemd-boot path", func(t *testing.T) {
		dir := t.TempDir()
		entriesDir := dir + "/boot/efi/loader/entries"
		if err := os.MkdirAll(entriesDir, 0o755); err != nil {
			t.Fatal(err)
		}
		entryPath := entriesDir + "/test.conf"
		input := "title Fedora Linux\noptions root=UUID=abc rw\n"
		if err := os.WriteFile(entryPath, []byte(input), 0o644); err != nil {
			t.Fatal(err)
		}
		n, err := post.EnsureLuksArgs(dir, testUUID)
		if err != nil {
			t.Fatalf("EnsureLuksArgs: %v", err)
		}
		if n != 1 {
			t.Errorf("expected 1 entry modified, got %d", n)
		}
		got, _ := os.ReadFile(entryPath)
		if !strings.Contains(string(got), wantArg) {
			t.Errorf("entry missing %q:\n%s", wantArg, got)
		}
	})

	t.Run("idempotent — does not duplicate rd.luks.name", func(t *testing.T) {
		dir := t.TempDir()
		entriesDir := dir + "/boot/loader/entries"
		if err := os.MkdirAll(entriesDir, 0o755); err != nil {
			t.Fatal(err)
		}
		entryPath := entriesDir + "/test.conf"
		input := "title Fedora Linux\noptions root=UUID=abc rw " + wantArg + "\n"
		if err := os.WriteFile(entryPath, []byte(input), 0o644); err != nil {
			t.Fatal(err)
		}
		n, err := post.EnsureLuksArgs(dir, testUUID)
		if err != nil {
			t.Fatalf("EnsureLuksArgs: %v", err)
		}
		if n != 0 {
			t.Errorf("expected 0 entries modified (idempotent), got %d", n)
		}
		got, _ := os.ReadFile(entryPath)
		count := strings.Count(string(got), wantArg)
		if count != 1 {
			t.Errorf("expected arg to appear once, got %d times:\n%s", count, got)
		}
	})

	t.Run("empty UUID is a no-op", func(t *testing.T) {
		dir := t.TempDir()
		n, err := post.EnsureLuksArgs(dir, "")
		if err != nil {
			t.Fatalf("EnsureLuksArgs with empty UUID: %v", err)
		}
		if n != 0 {
			t.Errorf("expected 0, got %d", n)
		}
	})

	t.Run("patches all entries in directory", func(t *testing.T) {
		dir := t.TempDir()
		entriesDir := filepath.Join(dir, "boot", "loader", "entries")
		if err := os.MkdirAll(entriesDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"entry1.conf", "entry2.conf", "entry3.conf"} {
			input := "title Fedora Linux\noptions root=UUID=abc rw\n"
			if err := os.WriteFile(filepath.Join(entriesDir, name), []byte(input), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		n, err := post.EnsureLuksArgs(dir, testUUID)
		if err != nil {
			t.Fatalf("EnsureLuksArgs: %v", err)
		}
		if n != 3 {
			t.Errorf("expected 3 entries modified, got %d", n)
		}
	})

	t.Run("patches both grub and systemd-boot paths simultaneously", func(t *testing.T) {
		dir := t.TempDir()
		for _, sub := range []string{"boot/loader/entries", "boot/efi/loader/entries"} {
			entriesDir := filepath.Join(dir, sub)
			if err := os.MkdirAll(entriesDir, 0o755); err != nil {
				t.Fatal(err)
			}
			input := "title Fedora Linux\noptions root=UUID=abc rw\n"
			if err := os.WriteFile(filepath.Join(entriesDir, "entry.conf"), []byte(input), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		n, err := post.EnsureLuksArgs(dir, testUUID)
		if err != nil {
			t.Fatalf("EnsureLuksArgs: %v", err)
		}
		if n != 2 {
			t.Errorf("expected 2 entries modified (one per path), got %d", n)
		}
	})

	t.Run("uses rd.luks.name not rd.luks.uuid", func(t *testing.T) {
		// Regression test: rd.luks.uuid maps to /dev/mapper/luks-<UUID> which
		// systemd-gpt-auto-generator cannot find. Must use rd.luks.name=<UUID>=root.
		dir := t.TempDir()
		entriesDir := filepath.Join(dir, "boot", "loader", "entries")
		if err := os.MkdirAll(entriesDir, 0o755); err != nil {
			t.Fatal(err)
		}
		entryPath := filepath.Join(entriesDir, "entry.conf")
		if err := os.WriteFile(entryPath, []byte("title Fedora Linux\noptions root=UUID=abc rw\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := post.EnsureLuksArgs(dir, testUUID)
		if err != nil {
			t.Fatalf("EnsureLuksArgs: %v", err)
		}
		got, _ := os.ReadFile(entryPath)
		if strings.Contains(string(got), "rd.luks.uuid=") {
			t.Errorf("entry contains rd.luks.uuid= which is wrong; must use rd.luks.name=:\n%s", got)
		}
		if !strings.Contains(string(got), "rd.luks.name="+testUUID+"=root") {
			t.Errorf("entry missing rd.luks.name=<UUID>=root:\n%s", got)
		}
	})

	t.Run("does not modify entry without options line", func(t *testing.T) {
		dir := t.TempDir()
		entriesDir := filepath.Join(dir, "boot", "loader", "entries")
		if err := os.MkdirAll(entriesDir, 0o755); err != nil {
			t.Fatal(err)
		}
		const input = "title Fedora Linux\n# no options line\n"
		entryPath := filepath.Join(entriesDir, "entry.conf")
		if err := os.WriteFile(entryPath, []byte(input), 0o644); err != nil {
			t.Fatal(err)
		}
		n, err := post.EnsureLuksArgs(dir, testUUID)
		if err != nil {
			t.Fatalf("EnsureLuksArgs: %v", err)
		}
		if n != 0 {
			t.Errorf("expected 0 modifications for entry with no options line, got %d", n)
		}
		got, _ := os.ReadFile(entryPath)
		if string(got) != input {
			t.Errorf("entry was modified unexpectedly:\n%s", got)
		}
	})
}

func TestEnablePrintServices(t *testing.T) {
	t.Run("composefs-native", func(t *testing.T) {
		dir := t.TempDir()

		// Set up stub deploy etc dir.
		deployEtc := filepath.Join(dir, "state", "deploy", "abc123", "etc")
		if err := os.MkdirAll(filepath.Join(deployEtc, "systemd", "system"), 0o755); err != nil {
			t.Fatal(err)
		}

		// Override ComposeFsDeployEtcDirFn.
		old := post.ComposeFsDeployEtcDirFn
		post.ComposeFsDeployEtcDirFn = func(string) (string, error) { return deployEtc, nil }
		t.Cleanup(func() { post.ComposeFsDeployEtcDirFn = old })

		post.EnablePrintServices(dir)

		wantsDir := filepath.Join(deployEtc, "systemd", "system", "multi-user.target.wants")
		for _, svc := range []string{"cups-browsed.service", "avahi-daemon.service", "ipp-usb.service"} {
			link := filepath.Join(wantsDir, svc)
			if _, err := os.Lstat(link); err != nil {
				t.Errorf("expected symlink for %s in deploy etc, got: %v", svc, err)
			}
		}
	})

	t.Run("ostree", func(t *testing.T) {
		dir := t.TempDir()

		// Create /ostree/ dir so isComposeFsNative returns false.
		if err := os.MkdirAll(filepath.Join(dir, "ostree"), 0o755); err != nil {
			t.Fatal(err)
		}

		// Intercept runner: ostree layout has /ostree but NO /state/deploy, so
		// "ls <sysroot>/state/deploy" must fail and "ls <sysroot>/ostree" succeed.
		origRunFn := runner.RunFn
		defer func() { runner.RunFn = origRunFn }()
		runner.RunFn = func(_ io.Reader, _ string, args ...string) error {
			if len(args) > 0 && strings.HasSuffix(args[len(args)-1], filepath.Join("state", "deploy")) {
				return fmt.Errorf("no state/deploy (ostree layout)")
			}
			return nil
		}

		// Override DeploymentDirFn.
		origFn := post.DeploymentDirFn
		defer func() { post.DeploymentDirFn = origFn }()
		deployDir := filepath.Join(dir, "deploy")
		if err := os.MkdirAll(filepath.Join(deployDir, "etc", "systemd", "system"), 0o755); err != nil {
			t.Fatal(err)
		}
		post.DeploymentDirFn = func(string) (string, error) { return deployDir, nil }

		post.EnablePrintServices(dir)

		wantsDir := filepath.Join(deployDir, "etc", "systemd", "system", "multi-user.target.wants")
		for _, svc := range []string{"cups-browsed.service", "avahi-daemon.service", "ipp-usb.service"} {
			link := filepath.Join(wantsDir, svc)
			if _, err := os.Lstat(link); err != nil {
				t.Errorf("expected symlink for %s in deploy dir, got: %v", svc, err)
			}
		}
	})
}

// TestDefaultComposeFsDeployEtcDir_BLSEntry verifies that the BLS loader entry
// composefs= field is parsed correctly to find the deploy etc dir.
func TestDefaultComposeFsDeployEtcDir_BLSEntry(t *testing.T) {
	target := t.TempDir()
	const hash = "61b6b932abc"

	// Create the deploy etc dir.
	deployEtc := filepath.Join(target, "state", "deploy", hash, "etc")
	if err := os.MkdirAll(deployEtc, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create a BLS loader entry with composefs=<hash>.
	entriesDir := filepath.Join(target, "boot", "loader", "entries")
	if err := os.MkdirAll(entriesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	entry := "title Bluefin\nversion 1\noptions root=UUID=abc rw composefs=" + hash + " rhgb quiet\n"
	if err := os.WriteFile(filepath.Join(entriesDir, "entry.conf"), []byte(entry), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := post.DefaultComposeFsDeployEtcDir(target)
	if err != nil {
		t.Fatalf("DefaultComposeFsDeployEtcDir: %v", err)
	}
	if got != deployEtc {
		t.Errorf("got %q, want %q", got, deployEtc)
	}
}

// TestDefaultComposeFsDeployEtcDir_Fallback verifies the fallback to newest
// state/deploy entry when no BLS entry is present.
func TestDefaultComposeFsDeployEtcDir_Fallback(t *testing.T) {
	target := t.TempDir()

	// Create two deploy dirs; the second is newer.
	deployBase := filepath.Join(target, "state", "deploy")
	first := filepath.Join(deployBase, "oldhash123", "etc")
	second := filepath.Join(deployBase, "newhash456", "etc")
	if err := os.MkdirAll(first, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := post.DefaultComposeFsDeployEtcDir(target)
	if err != nil {
		t.Fatalf("DefaultComposeFsDeployEtcDir: %v", err)
	}
	// Should return one of the two deploy etc dirs (whichever is newest).
	if got != first && got != second {
		t.Errorf("got %q, expected one of %q or %q", got, first, second)
	}
}

// TestIsComposeFsNative_ComposeFsBackendAlsoCreatesOstree is the regression test
// for the composefs installer crash: `bootc install to-filesystem
// --composefs-backend` lays the deployment under <sysroot>/state/deploy/<hash>/
// but ALSO creates an /ostree directory. The earlier "/ostree absent" heuristic
// therefore mis-classified composefs-native installs as ostree, and WriteHostname
// crashed with `finding deployment dir: ostree admin --print-current-dir: exit
// status 1`. Detection must key off state/deploy, so /ostree being present must
// NOT defeat it. Uses the real runner against a real tmpdir (no stub).
func TestIsComposeFsNative_ComposeFsBackendAlsoCreatesOstree(t *testing.T) {
	dir := t.TempDir()
	// composefs-native layout as bootc actually produces it: BOTH present.
	if err := os.MkdirAll(filepath.Join(dir, "ostree"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "state", "deploy", "abc123"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !post.IsComposeFsNativeExported(dir) {
		t.Errorf("composefs-native (state/deploy present, /ostree also present) must be detected as composefs")
	}
}

// TestIsComposeFsNative_TraditionalOstree verifies a traditional ostree layout
// (/ostree/deploy, no /state/deploy) is NOT treated as composefs-native.
func TestIsComposeFsNative_TraditionalOstree(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "ostree", "deploy", "default"), 0o755); err != nil {
		t.Fatal(err)
	}
	if post.IsComposeFsNativeExported(dir) {
		t.Errorf("traditional ostree (no state/deploy) must NOT be detected as composefs-native")
	}
}

// TestDefaultDeploymentDir_PrintCurrentDirFails is the regression test for the
// installer crash: `ostree admin --print-current-dir` always exits 1 against a
// freshly-installed target (never booted, no booted-deployment state).
// DefaultDeploymentDir must fall back to a filesystem glob and return the
// single deployment directory created by `bootc install to-filesystem`.
func TestDefaultDeploymentDir_PrintCurrentDirFails(t *testing.T) {
	sysroot := t.TempDir()
	deployDir := filepath.Join(sysroot, "ostree", "deploy", "default", "deploy", "abc123.0")
	if err := os.MkdirAll(deployDir, 0o755); err != nil {
		t.Fatal(err)
	}

	mock := setupMockExec(t)
	mock.responses["ostree"] = struct {
		out []byte
		err error
	}{err: fmt.Errorf("exit status 1")}

	got, err := post.DefaultDeploymentDir(sysroot)
	if err != nil {
		t.Fatalf("DefaultDeploymentDir: unexpected error: %v", err)
	}
	if got != deployDir {
		t.Errorf("DefaultDeploymentDir = %q, want %q", got, deployDir)
	}
}

// TestDefaultDeploymentDir_PrintCurrentDirSucceeds verifies the happy path:
// when `ostree admin --print-current-dir` returns a valid path, it is used
// directly without touching the filesystem.
func TestDefaultDeploymentDir_PrintCurrentDirSucceeds(t *testing.T) {
	sysroot := t.TempDir()
	want := "/sysroot/ostree/deploy/default/deploy/deadbeef.0"

	mock := setupMockExec(t)
	mock.responses["ostree"] = struct {
		out []byte
		err error
	}{out: []byte(want + "\n")}

	got, err := post.DefaultDeploymentDir(sysroot)
	if err != nil {
		t.Fatalf("DefaultDeploymentDir: unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("DefaultDeploymentDir = %q, want %q", got, want)
	}
}

// TestDefaultDeploymentDir_NoDeploymentFound verifies that when ostree fails
// AND no deployment directory exists on disk, an error is returned.
func TestDefaultDeploymentDir_NoDeploymentFound(t *testing.T) {
	sysroot := t.TempDir() // empty — no ostree/deploy/...

	mock := setupMockExec(t)
	mock.responses["ostree"] = struct {
		out []byte
		err error
	}{err: fmt.Errorf("exit status 1")}

	_, err := post.DefaultDeploymentDir(sysroot)
	if err == nil {
		t.Fatal("DefaultDeploymentDir: expected error for empty sysroot, got nil")
	}
}

// TestCopyFlatpaks_RemovesInstallerApps verifies that CopyFlatpaks strips the
// known installer Flatpak app IDs from the target /var/lib/flatpak directory
// after the copy so they are not present on the installed system.
// Regression test for projectbluefin/fisherman PR #1.
func TestCopyFlatpaks_RemovesInstallerApps(t *testing.T) {
	mock := setupMockExec(t)
	target := t.TempDir()

	// Simulate "no flatpak data" so we skip the tar pipe but still exercise
	// the cleanup path (which runs after the copy regardless).
	mock.responses["du -sb /var/lib/flatpak"] = struct {
		out []byte
		err error
	}{out: []byte("0\t/var/lib/flatpak\n")}

	// Resolve the expected dst path: composefs-native because no /ostree/ dir.
	dst := filepath.Join(target, "ostree", "deploy", "default", "var", "lib", "flatpak")

	// Pre-create installer app artifacts that should be removed.
	installerIDs := []string{
		"org.bootcinstaller.Installer",
		"org.bootcinstaller.Installer.Devel",
		"org.tunaos.Installer",
		"org.tunaos.Installer.Devel",
	}
	for _, id := range installerIDs {
		// app dir
		appDir := filepath.Join(dst, "app", id)
		if err := os.MkdirAll(appDir, 0o755); err != nil {
			t.Fatalf("mkdir appDir: %v", err)
		}
		// desktop entry
		desktopDir := filepath.Join(dst, "exports", "share", "applications")
		if err := os.MkdirAll(desktopDir, 0o755); err != nil {
			t.Fatalf("mkdir desktopDir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(desktopDir, id+".desktop"), []byte("[Desktop Entry]\n"), 0o644); err != nil {
			t.Fatalf("write desktop file: %v", err)
		}
		// dbus service file
		dbusDir := filepath.Join(dst, "exports", "share", "dbus-1", "services")
		if err := os.MkdirAll(dbusDir, 0o755); err != nil {
			t.Fatalf("mkdir dbusDir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dbusDir, id+".service"), []byte("[D-BUS Service]\n"), 0o644); err != nil {
			t.Fatalf("write dbus file: %v", err)
		}
	}

	if err := post.CopyFlatpaks(target, nil, ""); err != nil {
		t.Fatalf("CopyFlatpaks: %v", err)
	}

	// All installer artifacts must be gone.
	for _, id := range installerIDs {
		appDir := filepath.Join(dst, "app", id)
		if _, err := os.Stat(appDir); err == nil {
			t.Errorf("installer app dir still exists for %s: %s", id, appDir)
		}
		desktopFile := filepath.Join(dst, "exports", "share", "applications", id+".desktop")
		if _, err := os.Stat(desktopFile); err == nil {
			t.Errorf("installer desktop file still exists for %s: %s", id, desktopFile)
		}
		dbusFile := filepath.Join(dst, "exports", "share", "dbus-1", "services", id+".service")
		if _, err := os.Stat(dbusFile); err == nil {
			t.Errorf("installer dbus service still exists for %s: %s", id, dbusFile)
		}
	}
}

// TestCopyFlatpaks_PreservesNonInstallerApps verifies that CopyFlatpaks does NOT
// remove regular user apps — only the known installer app IDs are targeted.
func TestCopyFlatpaks_PreservesNonInstallerApps(t *testing.T) {
	mock := setupMockExec(t)
	target := t.TempDir()

	mock.responses["du -sb /var/lib/flatpak"] = struct {
		out []byte
		err error
	}{out: []byte("0\t/var/lib/flatpak\n")}

	dst := filepath.Join(target, "ostree", "deploy", "default", "var", "lib", "flatpak")

	// Regular app that must NOT be removed.
	userAppDir := filepath.Join(dst, "app", "org.mozilla.firefox")
	if err := os.MkdirAll(userAppDir, 0o755); err != nil {
		t.Fatalf("mkdir userAppDir: %v", err)
	}

	if err := post.CopyFlatpaks(target, nil, ""); err != nil {
		t.Fatalf("CopyFlatpaks: %v", err)
	}

	// org.mozilla.firefox must still be present.
	if _, err := os.Stat(userAppDir); err != nil {
		t.Errorf("non-installer app dir was unexpectedly removed: %v", err)
	}
}

// TestCopyFlatpaks_CleanupIdempotentWhenAppsMissing verifies that cleanup is
// a no-op (no error) when no installer app dirs exist in the target.
func TestCopyFlatpaks_CleanupIdempotentWhenAppsMissing(t *testing.T) {
	mock := setupMockExec(t)
	target := t.TempDir()

	mock.responses["du -sb /var/lib/flatpak"] = struct {
		out []byte
		err error
	}{out: []byte("0\t/var/lib/flatpak\n")}

	// No app dirs pre-created — cleanup should be silent.
	if err := post.CopyFlatpaks(target, nil, ""); err != nil {
		t.Fatalf("CopyFlatpaks: %v", err)
	}
}

// TestCopyFlatpaks_RemovesInstallerApps_CustomFlatpakVarPath verifies cleanup
// also works when a custom flatpakVarPath is set (e.g. GnomeOS/Dakota layout).
func TestCopyFlatpaks_RemovesInstallerApps_CustomFlatpakVarPath(t *testing.T) {
	mock := setupMockExec(t)
	target := t.TempDir()

	mock.responses["du -sb /var/lib/flatpak"] = struct {
		out []byte
		err error
	}{out: []byte("0\t/var/lib/flatpak\n")}

	flatpakVarPath := "state/os/default/var"
	dst := filepath.Join(target, flatpakVarPath, "lib", "flatpak")

	appID := "org.tunaos.Installer"
	appDir := filepath.Join(dst, "app", appID)
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatalf("mkdir appDir: %v", err)
	}

	if err := post.CopyFlatpaks(target, nil, flatpakVarPath); err != nil {
		t.Fatalf("CopyFlatpaks: %v", err)
	}

	if _, err := os.Stat(appDir); err == nil {
		t.Errorf("installer app dir still exists at custom flatpakVarPath: %s", appDir)
	}
}

// TestIsNoSpace covers both shapes an ENOSPC failure arrives in: a wrapped
// syscall error (tar extract, translated from its stderr) and raw strerror
// text scraped from a subprocess (tee, cp, mkdir).
func TestIsNoSpace(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"wrapped ENOSPC", fmt.Errorf("tar extract: %w", syscall.ENOSPC), true},
		{"bare ENOSPC", syscall.ENOSPC, true},
		{"strerror text", errors.New("tee: /mnt/t/etc/hostname: No space left on device"), true},
		{"unrelated", errors.New("exit status 2"), false},
		{"permission", syscall.EACCES, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := post.IsNoSpace(tc.err); got != tc.want {
				t.Errorf("IsNoSpace(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
