package disk

import (
	"io"
	"testing"

	"github.com/tuna-os/fisherman/internal/runner"
)

// formatPartition is unexported and otherwise reached only through
// ApplyCustomLayout, which mounts real devices — so it is tested here, inside
// the package, rather than through the exported surface in the _test package.

type recordedCall struct {
	name string
	args []string
}

func recordRuns(t *testing.T) *[]recordedCall {
	t.Helper()
	var calls []recordedCall
	runner.RunFn = func(_ io.Reader, name string, args ...string) error {
		calls = append(calls, recordedCall{name: name, args: args})
		return nil
	}
	t.Cleanup(func() { runner.RunFn = runner.DefaultRun })
	return &calls
}

// The custom-layout path and the auto-partition path must agree about ext4.
// FormatRoot passes -O verity because bootc's --composefs-backend enables
// verity on individual objects as it writes them, and ext4 only permits that
// when the feature was set at mkfs time — it cannot be turned on later.
// formatPartition did not pass it, so composefs worked on the auto path and
// failed on the custom path with
//
//	Finalizing object tempfile: Enabling verity on tmpfile:
//	Filesystem does not support fs-verity
//
// deep in the deploy, AFTER the target was formatted and the image pulled.
// Hosts that install into pre-existing partitions use the custom path
// exclusively, so composefs was unreachable for exactly the callers most
// likely to need it.
func TestFormatPartitionExt4EnablesVerity(t *testing.T) {
	calls := recordRuns(t)
	if err := formatPartition("/dev/sda3", "ext4"); err != nil {
		t.Fatalf("formatPartition: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected 1 call, got %d: %+v", len(*calls), *calls)
	}
	c := (*calls)[0]
	if c.name != "mkfs.ext4" {
		t.Fatalf("name = %q, want mkfs.ext4", c.name)
	}
	sawVerity := false
	for i, a := range c.args {
		if a == "-O" && i+1 < len(c.args) && c.args[i+1] == "verity" {
			sawVerity = true
		}
	}
	if !sawVerity {
		t.Errorf("ext4 custom-mount format is missing -O verity; composefs installs "+
			"into pre-existing partitions fail mid-deploy. args = %v", c.args)
	}
}
