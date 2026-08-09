package post

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/tuna-os/fisherman/internal/progress"
)

// AudioDevice represents a detected PipeWire/ALSA audio node.
type AudioDevice struct {
	NodeName    string // e.g. "alsa_output.pci-0000_c5_00.6.analog-stereo"
	Description string // e.g. "Family 17h/19h HD Audio Controller"
	Class       string // "Audio/Sink" or "Audio/Source"
}

// friendlyName maps ugly ALSA descriptions to human-friendly names.
// Returns "" if no rename is needed (already reasonable).
func friendlyName(dev AudioDevice) string {
	desc := dev.Description
	name := dev.NodeName

	// Pattern-based renames (order matters — first match wins)
	renames := []struct {
		pattern string
		result  string
	}{
		// Laptop built-in audio (AMD/Intel HD Audio controllers)
		{`(?i)Family \d+h(/\d+h)? HD Audio Controller.*Analog Stereo`, "Laptop Speakers"},
		{`(?i)HD Audio Controller.*Analog Stereo`, "Laptop Speakers"},
		{`(?i)Rembrandt.*Analog Stereo`, "Laptop Speakers"},
		{`(?i)Renoir.*Analog Stereo`, "Laptop Speakers"},

		// Intel HDA variants
		{`(?i)Built-in Audio.*Analog Stereo`, "Laptop Speakers"},
		{`(?i)Internal Audio.*Analog Stereo`, "Laptop Speakers"},

		// HDMI / DisplayPort audio
		{`(?i)HDMI\s*/\s*DisplayPort\s*\d*`, "Monitor Audio"},
		{`(?i)HDMI\s+\d+`, "Monitor Audio"},

		// USB microphones — strip "USB Audio" prefix noise
		{`(?i)USB-Audio.*-\s*(.+)`, ""},

		// Bluetooth — already has reasonable names usually
	}

	fullDesc := desc
	if strings.Contains(name, "analog-stereo") && !strings.Contains(desc, "Stereo") {
		fullDesc = desc + " Analog Stereo"
	}

	for _, r := range renames {
		re := regexp.MustCompile(r.pattern)
		if re.MatchString(fullDesc) || re.MatchString(desc) {
			if r.result != "" {
				// For sinks vs sources, disambiguate
				if r.result == "Laptop Speakers" && isSource(dev) {
					return "Laptop Microphone"
				}
				return r.result
			}
		}
	}

	// If the description is already short and sensible, keep it
	if len(desc) < 30 {
		return ""
	}

	// Strip common verbose suffixes
	cleaned := desc
	cleaned = regexp.MustCompile(`(?i)\s*Analog Stereo$`).ReplaceAllString(cleaned, "")
	cleaned = regexp.MustCompile(`(?i)\s*Digital Stereo$`).ReplaceAllString(cleaned, "")
	cleaned = regexp.MustCompile(`(?i)\s*\(.*\)$`).ReplaceAllString(cleaned, "")
	cleaned = strings.TrimSpace(cleaned)

	if cleaned != desc && len(cleaned) < len(desc) {
		return cleaned
	}
	return ""
}

// shouldHide returns true for devices that should be disabled — they
// confuse normal users and are rarely used on consumer hardware.
func shouldHide(dev AudioDevice) bool {
	name := strings.ToLower(dev.NodeName)
	desc := strings.ToLower(dev.Description)

	hidePatterns := []string{
		"iec958",                  // S/PDIF digital output
		"spdif",                   // S/PDIF alternate spelling
		"s/pdif",                  // S/PDIF with slash
		"digital output",          // generic digital
		"digital stereo (iec958)", // PulseAudio-style
		"pro audio",               // raw multi-channel, not for consumers
		"multichannel",            // multi-channel raw
	}

	for _, p := range hidePatterns {
		if strings.Contains(name, p) || strings.Contains(desc, p) {
			return true
		}
	}

	// Hide "Monitor of ..." virtual loopback sources
	if isSource(dev) && strings.HasPrefix(desc, "monitor of ") {
		return true
	}

	return false
}

func isSource(dev AudioDevice) bool {
	return strings.Contains(dev.Class, "Source") ||
		strings.Contains(dev.NodeName, "input") ||
		strings.Contains(dev.NodeName, "source")
}

// GenerateAudioConfig detects audio devices on the live system and writes
// WirePlumber rules to rename confusing devices and hide useless ones.
// The config is placed at /etc/wireplumber/wireplumber.conf.d/ in the target.
//
// This is a system-level fix — no GNOME extensions needed.
func GenerateAudioConfig(targetRoot string) error {
	devices, err := detectAudioDevices()
	if err != nil {
		return fmt.Errorf("detecting audio devices: %w", err)
	}

	if len(devices) == 0 {
		progress.Info("No audio devices detected — skipping audio config")
		return nil
	}

	var rules []string
	renamed := 0
	hidden := 0

	for _, dev := range devices {
		if shouldHide(dev) {
			rules = append(rules, formatHideRule(dev))
			hidden++
			continue
		}

		friendly := friendlyName(dev)
		if friendly != "" {
			rules = append(rules, formatRenameRule(dev, friendly))
			renamed++
		}
	}

	if len(rules) == 0 {
		progress.Info("Audio devices already have sensible names")
		return nil
	}

	// Resolve the correct /etc for the target layout.
	// composefs-native: writable /etc lives under state/deploy/<hash>/etc.
	// ostree: writable /etc lives in the ostree deployment subtree.
	var etcBase string
	if isComposeFsNative(targetRoot) {
		var err error
		etcBase, err = ComposeFsDeployEtcDirFn(targetRoot)
		if err != nil {
			return fmt.Errorf("finding composefs deploy etc for audio config: %w", err)
		}
	} else {
		etcBase = filepath.Join(targetRoot, "etc")
	}

	// Write WirePlumber config
	confDir := filepath.Join(etcBase, "wireplumber", "wireplumber.conf.d")
	if err := os.MkdirAll(confDir, 0755); err != nil {
		return fmt.Errorf("creating wireplumber conf dir: %w", err)
	}

	content := generateWirePlumberConf(rules)
	confPath := filepath.Join(confDir, "60-friendly-audio-names.conf")
	if err := os.WriteFile(confPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("writing wireplumber config: %w", err)
	}

	progress.Info(fmt.Sprintf("Audio cleanup: renamed %d devices, hidden %d useless outputs", renamed, hidden))
	return nil
}

// ApplyAudioConfigLive detects audio devices and applies friendly names
// immediately on the live session. Writes to the user-level WirePlumber
// config (~/.config/wireplumber/wireplumber.conf.d/) and restarts
// WirePlumber so changes take effect without reboot.
//
// Call this BEFORE install starts so the user sees clean audio names
// while using the installer. The same rules are later persisted to the
// target during post-install via GenerateAudioConfig().
func ApplyAudioConfigLive() error {
	devices, err := detectAudioDevices()
	if err != nil {
		return fmt.Errorf("detecting audio devices: %w", err)
	}

	if len(devices) == 0 {
		return nil
	}

	var rules []string
	for _, dev := range devices {
		if shouldHide(dev) {
			rules = append(rules, formatHideRule(dev))
			continue
		}
		if friendly := friendlyName(dev); friendly != "" {
			rules = append(rules, formatRenameRule(dev, friendly))
		}
	}

	if len(rules) == 0 {
		return nil
	}

	// Write to user-level config (no root needed, works in live session)
	home := os.Getenv("HOME")
	if home == "" {
		home = "/home/liveuser"
	}
	confDir := filepath.Join(home, ".config", "wireplumber", "wireplumber.conf.d")
	if err := os.MkdirAll(confDir, 0755); err != nil {
		return fmt.Errorf("creating user wireplumber conf dir: %w", err)
	}

	content := generateWirePlumberConf(rules)
	confPath := filepath.Join(confDir, "60-friendly-audio-names.conf")
	if err := os.WriteFile(confPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("writing user wireplumber config: %w", err)
	}

	// Also write to system /etc/ so it persists for the live session's
	// system-level WirePlumber (covers both user and system instances)
	sysDir := "/etc/wireplumber/wireplumber.conf.d"
	if err := os.MkdirAll(sysDir, 0755); err == nil {
		_ = os.WriteFile(filepath.Join(sysDir, "60-friendly-audio-names.conf"), []byte(content), 0644)
	}

	// Restart WirePlumber to pick up new rules immediately
	_ = Exec.Command("systemctl", "--user", "restart", "wireplumber").Run()

	progress.Info("Applied friendly audio device names to live session")
	return nil
}

// detectAudioDevices uses pw-cli to list all audio nodes.
func detectAudioDevices() ([]AudioDevice, error) {
	// pw-cli ls Node gives us all nodes with their properties
	out, err := Exec.Command("pw-cli", "ls", "Node").Output()
	if err != nil {
		// Fallback: try pactl (works even without PipeWire session)
		return detectAudioDevicesPactl()
	}
	return parsePwCliOutput(string(out)), nil
}

// parsePwCliOutput parses `pw-cli ls Node` output into AudioDevice structs.
func parsePwCliOutput(output string) []AudioDevice {
	var devices []AudioDevice
	var current *AudioDevice

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)

		// New node block starts with "id N, ..."
		if strings.HasPrefix(line, "id ") {
			if current != nil && current.NodeName != "" {
				devices = append(devices, *current)
			}
			current = &AudioDevice{}
			continue
		}

		if current == nil {
			continue
		}

		if strings.Contains(line, "node.name") {
			current.NodeName = extractPwValue(line)
		} else if strings.Contains(line, "node.description") {
			current.Description = extractPwValue(line)
		} else if strings.Contains(line, "media.class") {
			current.Class = extractPwValue(line)
		}
	}

	// Don't forget last entry
	if current != nil && current.NodeName != "" {
		devices = append(devices, *current)
	}

	// Filter to only audio sinks and sources
	var audioDevices []AudioDevice
	for _, d := range devices {
		if strings.Contains(d.Class, "Audio/Sink") || strings.Contains(d.Class, "Audio/Source") {
			audioDevices = append(audioDevices, d)
		}
	}
	return audioDevices
}

func extractPwValue(line string) string {
	// Format: '    node.name = "alsa_output.pci-..."'
	parts := strings.SplitN(line, "=", 2)
	if len(parts) < 2 {
		return ""
	}
	val := strings.TrimSpace(parts[1])
	val = strings.Trim(val, "\"")
	return val
}

// detectAudioDevicesPactl is a fallback using pactl.
func detectAudioDevicesPactl() ([]AudioDevice, error) {
	var devices []AudioDevice

	// Get sinks
	out, err := Exec.Command("pactl", "list", "sinks", "short").Output()
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				desc := getDeviceDescription("sink", fields[1])
				devices = append(devices, AudioDevice{
					NodeName:    fields[1],
					Description: desc,
					Class:       "Audio/Sink",
				})
			}
		}
	}

	// Get sources
	out, err = Exec.Command("pactl", "list", "sources", "short").Output()
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				desc := getDeviceDescription("source", fields[1])
				devices = append(devices, AudioDevice{
					NodeName:    fields[1],
					Description: desc,
					Class:       "Audio/Source",
				})
			}
		}
	}

	if len(devices) == 0 {
		return nil, fmt.Errorf("no audio devices found via pactl")
	}
	return devices, nil
}

// getDeviceDescription fetches the description for a specific sink/source.
func getDeviceDescription(kind, name string) string {
	out, err := Exec.Command("pactl", "--format=json", "list", kind+"s").Output()
	if err != nil {
		return name // fallback to raw name
	}
	// Simple extraction: find the description near our device name
	lines := strings.Split(string(out), "\n")
	found := false
	for _, line := range lines {
		if strings.Contains(line, name) {
			found = true
		}
		if found && strings.Contains(line, "description") {
			val := extractPwValue(line)
			if val != "" {
				return val
			}
		}
	}
	return name
}

// formatRenameRule creates a WirePlumber rule to rename a device.
func formatRenameRule(dev AudioDevice, friendly string) string {
	return fmt.Sprintf(`  {
    matches = [
      {
        node.name = %q
      }
    ]
    actions = {
      update-props = {
        node.description = %q
        node.nick        = %q
      }
    }
  }`, dev.NodeName, friendly, friendly)
}

// formatHideRule creates a WirePlumber rule to disable a device.
func formatHideRule(dev AudioDevice) string {
	return fmt.Sprintf(`  {
    matches = [
      {
        node.name = %q
      }
    ]
    actions = {
      update-props = {
        node.disabled = true
      }
    }
  }`, dev.NodeName)
}

// generateWirePlumberConf wraps rules in proper WirePlumber 0.5 config format.
func generateWirePlumberConf(rules []string) string {
	var sb strings.Builder
	sb.WriteString(`# Auto-generated by fisherman installer
# Gives audio devices human-friendly names and hides confusing outputs
# like S/PDIF that normal users never use.
#
# To undo: delete this file and restart WirePlumber
# Location: /etc/wireplumber/wireplumber.conf.d/60-friendly-audio-names.conf

monitor.alsa.rules = [
`)
	sb.WriteString(strings.Join(rules, "\n"))
	sb.WriteString("\n]\n")
	return sb.String()
}
