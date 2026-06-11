package post

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/fisherman/internal/runner"
)

func TestFriendlyName(t *testing.T) {
	tests := []struct {
		name     string
		dev      AudioDevice
		expected string
	}{
		{
			name:     "AMD HD Audio Controller → Laptop Speakers",
			dev:      AudioDevice{NodeName: "alsa_output.pci-0000_c5_00.6.analog-stereo", Description: "Family 17h/19h HD Audio Controller Analog Stereo", Class: "Audio/Sink"},
			expected: "Laptop Speakers",
		},
		{
			name:     "AMD HD Audio source → Laptop Microphone",
			dev:      AudioDevice{NodeName: "alsa_input.pci-0000_c5_00.6.analog-stereo", Description: "Family 17h/19h HD Audio Controller Analog Stereo", Class: "Audio/Source"},
			expected: "Laptop Microphone",
		},
		{
			name:     "Rembrandt sink → Laptop Speakers",
			dev:      AudioDevice{NodeName: "alsa_output.pci-0000_65_00.1.analog-stereo", Description: "Rembrandt Radeon HD Audio Controller Analog Stereo", Class: "Audio/Sink"},
			expected: "Laptop Speakers",
		},
		{
			name:     "HDMI/DisplayPort → Monitor Audio",
			dev:      AudioDevice{NodeName: "alsa_output.pci-0000_c5_00.1.hdmi-stereo", Description: "HDMI / DisplayPort 2", Class: "Audio/Sink"},
			expected: "Monitor Audio",
		},
		{
			name:     "Built-in Audio → Laptop Speakers",
			dev:      AudioDevice{NodeName: "alsa_output.pci-0000_00_1f.3.analog-stereo", Description: "Built-in Audio Analog Stereo", Class: "Audio/Sink"},
			expected: "Laptop Speakers",
		},
		{
			name:     "short description unchanged",
			dev:      AudioDevice{NodeName: "alsa_output.usb-Blue_Yeti-00.analog-stereo", Description: "Blue Yeti", Class: "Audio/Sink"},
			expected: "", // no rename needed
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := friendlyName(tc.dev)
			if got != tc.expected {
				t.Errorf("friendlyName(%q) = %q, want %q", tc.dev.Description, got, tc.expected)
			}
		})
	}
}

func TestShouldHide(t *testing.T) {
	tests := []struct {
		name   string
		dev    AudioDevice
		hidden bool
	}{
		{
			name:   "S/PDIF output hidden",
			dev:    AudioDevice{NodeName: "alsa_output.pci-0000_c5_00.6.iec958-stereo", Description: "S/PDIF Digital Stereo Output", Class: "Audio/Sink"},
			hidden: true,
		},
		{
			name:   "Pro Audio hidden",
			dev:    AudioDevice{NodeName: "alsa_output.pci-0000_c5_00.6.pro-audio", Description: "Pro Audio", Class: "Audio/Sink"},
			hidden: true,
		},
		{
			name:   "Monitor of source hidden",
			dev:    AudioDevice{NodeName: "alsa_input.pci-0000_c5_00.6.analog-stereo.monitor", Description: "Monitor of Family 17h", Class: "Audio/Source"},
			hidden: true,
		},
		{
			name:   "Normal analog output not hidden",
			dev:    AudioDevice{NodeName: "alsa_output.pci-0000_c5_00.6.analog-stereo", Description: "Family 17h/19h HD Audio Controller Analog Stereo", Class: "Audio/Sink"},
			hidden: false,
		},
		{
			name:   "USB mic not hidden",
			dev:    AudioDevice{NodeName: "alsa_input.usb-Elgato_Wave_3-00.mono-fallback", Description: "Elgato Wave:3", Class: "Audio/Source"},
			hidden: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldHide(tc.dev)
			if got != tc.hidden {
				t.Errorf("shouldHide(%q) = %v, want %v", tc.dev.NodeName, got, tc.hidden)
			}
		})
	}
}

func TestParsePwCliOutput(t *testing.T) {
	sample := `	id 42, type PipeWire:Interface:Node/3, version 3
		node.name = "alsa_output.pci-0000_c5_00.6.analog-stereo"
		node.description = "Family 17h/19h HD Audio Controller Analog Stereo"
		media.class = "Audio/Sink"
	id 43, type PipeWire:Interface:Node/3, version 3
		node.name = "alsa_output.pci-0000_c5_00.6.iec958-stereo"
		node.description = "S/PDIF Digital Output"
		media.class = "Audio/Sink"
	id 44, type PipeWire:Interface:Node/3, version 3
		node.name = "alsa_input.pci-0000_c5_00.6.analog-stereo"
		node.description = "Family 17h/19h HD Audio Controller Analog Stereo"
		media.class = "Audio/Source"
	id 50, type PipeWire:Interface:Node/3, version 3
		node.name = "v4l2_output.pci-0000_c5_00.3-usb-0"
		node.description = "USB Camera"
		media.class = "Video/Source"
`

	devices := parsePwCliOutput(sample)
	if len(devices) != 3 {
		t.Fatalf("expected 3 audio devices, got %d", len(devices))
	}

	if devices[0].Description != "Family 17h/19h HD Audio Controller Analog Stereo" {
		t.Errorf("first device description = %q", devices[0].Description)
	}
	if devices[1].NodeName != "alsa_output.pci-0000_c5_00.6.iec958-stereo" {
		t.Errorf("second device name = %q", devices[1].NodeName)
	}
	// Video source should be filtered out
	for _, d := range devices {
		if strings.Contains(d.Class, "Video") {
			t.Error("video device should have been filtered out")
		}
	}
}

func TestGenerateAudioConfigWritesFile(t *testing.T) {
	tmpDir := t.TempDir()

	// Set up a stub composefs deploy etc dir.
	deployEtc := filepath.Join(tmpDir, "state", "deploy", "abc123", "etc")
	if err := os.MkdirAll(deployEtc, 0o755); err != nil {
		t.Fatal(err)
	}
	oldFn := ComposeFsDeployEtcDirFn
	ComposeFsDeployEtcDirFn = func(string) (string, error) { return deployEtc, nil }
	defer func() { ComposeFsDeployEtcDirFn = oldFn }()

	// Mock the executor to return known pw-cli output
	origExec := Exec
	defer func() { Exec = origExec }()

	Exec = &mockAudioExecutor{
		pwOutput: `	id 42, type PipeWire:Interface:Node/3, version 3
		node.name = "alsa_output.pci-0000_c5_00.6.analog-stereo"
		node.description = "Family 17h/19h HD Audio Controller Analog Stereo"
		media.class = "Audio/Sink"
	id 43, type PipeWire:Interface:Node/3, version 3
		node.name = "alsa_output.pci-0000_c5_00.6.iec958-stereo"
		node.description = "S/PDIF Digital Output"
		media.class = "Audio/Sink"
`,
	}

	err := GenerateAudioConfig(tmpDir)
	if err != nil {
		t.Fatalf("GenerateAudioConfig() error: %v", err)
	}

	confPath := filepath.Join(deployEtc, "wireplumber", "wireplumber.conf.d", "60-friendly-audio-names.conf")
	data, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatalf("config file not written: %v", err)
	}

	content := string(data)

	// Should contain rename rule
	if !strings.Contains(content, "Laptop Speakers") {
		t.Error("expected 'Laptop Speakers' rename in config")
	}

	// Should contain hide rule for S/PDIF
	if !strings.Contains(content, "node.disabled = true") {
		t.Error("expected hide rule for S/PDIF")
	}

	// Should be valid WirePlumber format
	if !strings.Contains(content, "monitor.alsa.rules") {
		t.Error("expected monitor.alsa.rules header")
	}
}

// mockAudioExecutor returns canned pw-cli output.
type mockAudioExecutor struct {
	pwOutput string
}

func (m *mockAudioExecutor) Command(name string, args ...string) runner.Command {
	return &mockAudioCommand{name: name, output: m.pwOutput}
}

type mockAudioCommand struct {
	name   string
	output string
}

func (c *mockAudioCommand) Run() error                { return nil }
func (c *mockAudioCommand) Start() error              { return nil }
func (c *mockAudioCommand) Wait() error               { return nil }
func (c *mockAudioCommand) SetStdin(r io.Reader)      {}
func (c *mockAudioCommand) SetStdout(w io.Writer)     {}
func (c *mockAudioCommand) SetStderr(w io.Writer)     {}
func (c *mockAudioCommand) Output() ([]byte, error) {
	if c.name == "pw-cli" {
		return []byte(c.output), nil
	}
	return nil, os.ErrNotExist
}
