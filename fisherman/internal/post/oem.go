package post

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tuna-os/fisherman/internal/progress"
	"github.com/tuna-os/fisherman/internal/runner"
)

// OEM hardware vendor detection and first-boot package provisioning.
// Detects the laptop/desktop vendor from DMI data and writes a systemd
// user service that installs vendor-specific packages (via brew) on the
// user's first login.

// vendorPackages maps DMI vendor names to brew packages to install.
var vendorPackages = map[string][]string{
	"asus": {
		"ublue-os/tap/asusctl-linux",
		"ublue-os/tap/rog-control-center-linux",
	},
	"framework": {
		"ublue-os/tap/framework-tool",
	},
}

// vendorServices maps DMI vendor names to systemd services to enable.
var vendorServices = map[string][]string{
	"asus": {
		"asusd.service",
	},
}

// vendorMenuIcons maps vendor to their custom-command-menu icon name.
// Only vendors with a recognizable non-wordmark symbol get an icon.
var vendorMenuIcons = map[string]string{
	"asus":      "asus-rog-symbolic",
	"framework": "framework-symbolic",
	"dell":      "dell-symbolic",
	"alienware": "alienware-symbolic",
	"hp":        "hp-symbolic",
	"system76":  "system76-symbolic",
	"razer":     "razer-symbolic",
	"msi":       "msi-symbolic",
	"nvidia":    "nvidia-symbolic",
	"arm":       "arm-symbolic",
	"legion":    "legion-symbolic",
	"yoga":      "yoga-symbolic",
	"thinkpad":  "thinkpad-symbolic",
	"thinkpadx": "thinkpadx-symbolic",
}

// detectVendor reads the system vendor from DMI sysfs.
func detectVendor() string {
	data, err := os.ReadFile("/sys/devices/virtual/dmi/id/sys_vendor")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// detectSubBrand reads the product name from DMI sysfs and returns a
// sub-brand if one is recognized (e.g. "Legion", "Yoga", "Alienware").
func detectSubBrand() string {
	data, err := os.ReadFile("/sys/devices/virtual/dmi/id/product_name")
	if err != nil {
		return ""
	}
	product := strings.ToLower(strings.TrimSpace(string(data)))

	subBrands := []struct {
		keyword string
		brand   string
	}{
		{"legion", "legion"},
		{"yoga", "yoga"},
		{"alienware", "alienware"},
		{"thinkpad x", "thinkpadx"},
		{"thinkpad", "thinkpad"},
	}

	for _, sb := range subBrands {
		if strings.Contains(product, sb.keyword) {
			return sb.brand
		}
	}
	return ""
}

// normalizeVendor maps raw DMI vendor strings to a canonical short name.
func normalizeVendor(raw string) string {
	vendorMap := map[string]string{
		"asustek computer inc.":              "asus",
		"asus":                               "asus",
		"framework":                          "framework",
		"tuxedo":                             "tuxedo",
		"tuxedo computers":                   "tuxedo",
		"tuxedo computers gmbh":              "tuxedo",
		"dell inc.":                          "dell",
		"dell":                               "dell",
		"lenovo":                             "lenovo",
		"hp":                                 "hp",
		"hewlett-packard":                    "hp",
		"hewlett packard":                    "hp",
		"system76":                           "system76",
		"razer":                              "razer",
		"micro-star international co., ltd.": "msi",
		"msi":                                "msi",
	}
	return vendorMap[strings.ToLower(strings.TrimSpace(raw))]
}

// InstallOEMPackages detects the hardware vendor and writes a first-boot
// systemd user service that installs vendor-specific packages via brew.
// Also enables any required system services in the target.
// Sets the vendor's logo as the shell menu icon for hardware confidence.
// distroID is used to name paths/services (defaults to "bootc" if empty).
// brewTap is an optional Homebrew tap to add before installing packages.
// Non-fatal: always returns nil (best-effort).
func InstallOEMPackages(target, distroID, brewTap string) error {
	if distroID == "" {
		distroID = "bootc"
	}
	raw := detectVendor()
	vendor := normalizeVendor(raw)

	if vendor != "" {
		pkgs, hasPkgs := vendorPackages[vendor]
		svcs, hasSvcs := vendorServices[vendor]

		// Write a first-boot systemd user service that installs brew packages.
		if hasPkgs && len(pkgs) > 0 {
			if err := writeOEMBrewService(target, vendor, pkgs, distroID, brewTap); err != nil {
				return fmt.Errorf("writing OEM brew service: %w", err)
			}
		}

		// Enable system-level services (e.g. asusd) in the target.
		if hasSvcs {
			for _, svc := range svcs {
				enableSystemService(target, svc)
			}
		}

		progress.Info(fmt.Sprintf("OEM setup for %s: %d packages queued, %d services enabled",
			vendor, len(pkgs), len(svcs)))
	}

	// Set menu icon. Priority: sub-brand > vendor > NVIDIA GPU > ARM arch.
	iconVendor := ""
	if sub := detectSubBrand(); sub != "" {
		if _, ok := vendorMenuIcons[sub]; ok {
			iconVendor = sub
		}
	}
	if iconVendor == "" {
		iconVendor = vendor
	}
	if _, ok := vendorMenuIcons[iconVendor]; !ok {
		if gpu := detectGPUVendor(); gpu != "" {
			iconVendor = gpu
		} else if arch := detectArch(); arch != "" {
			iconVendor = arch
		}
	}

	if iconVendor == "" {
		return nil
	}

	if icon, ok := vendorMenuIcons[iconVendor]; ok {
		if err := installVendorIcon(target, iconVendor); err != nil {
			progress.Info(fmt.Sprintf("Warning: could not install icon for %s: %v", iconVendor, err))
		}
		if err := writeMenuIconOverride(target, icon); err != nil {
			progress.Info(fmt.Sprintf("Warning: could not set menu icon for %s: %v", iconVendor, err))
		}
		progress.Info(fmt.Sprintf("Menu icon set to %s", icon))
	}

	return nil
}

// writeOEMBrewService creates a systemd user unit + script that runs once
// on first login to install vendor-specific brew packages.
func writeOEMBrewService(target string, vendor string, pkgs []string, distroID, brewTap string) error {
	// Resolve target paths (composefs-native vs ostree).
	var etcDir string
	if isComposeFsNative(target) {
		var err error
		etcDir, err = ComposeFsDeployEtcDirFn(target)
		if err != nil {
			return fmt.Errorf("finding composefs deploy etc: %w", err)
		}
	} else {
		deployDir, err := DeploymentDirFn(target)
		if err != nil {
			return fmt.Errorf("finding deployment dir: %w", err)
		}
		etcDir = filepath.Join(deployDir, "etc")
	}

	// Write the install script.
	scriptDir := filepath.Join(etcDir, distroID, "oem")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", scriptDir, err)
	}

	brewInstallCmds := ""
	for _, pkg := range pkgs {
		// Determine if it's a cask (ends with -linux) or formula
		if strings.HasSuffix(pkg, "-linux") {
			brewInstallCmds += fmt.Sprintf("brew install --cask %s 2>/dev/null || true\n", pkg)
		} else {
			brewInstallCmds += fmt.Sprintf("brew install %s 2>/dev/null || true\n", pkg)
		}
	}

	tapCmd := ""
	if brewTap != "" {
		tapCmd = fmt.Sprintf("brew tap %s 2>/dev/null || true\n", brewTap)
	}

	script := fmt.Sprintf(`#!/bin/bash
# OEM package installer for %s hardware.
# Auto-generated by fisherman during installation.
# Runs once on first login, then disables itself.

set -euo pipefail

MARKER="$HOME/.config/%s/oem-setup-done"
if [[ -f "$MARKER" ]]; then
    exit 0
fi

# Wait for brew to be available (ujust may set it up)
for i in $(seq 1 30); do
    if command -v brew &>/dev/null; then
        break
    fi
    sleep 2
done

if ! command -v brew &>/dev/null; then
    echo "OEM setup: brew not available, skipping"
    exit 0
fi

%s# Install vendor packages
%s
# Mark as done
mkdir -p "$(dirname "$MARKER")"
touch "$MARKER"

# Disable the service so it doesn't run again
systemctl --user disable %s-oem-setup.service 2>/dev/null || true
`, vendor, distroID, tapCmd, brewInstallCmds, distroID)

	scriptPath := filepath.Join(scriptDir, "install-packages.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		return fmt.Errorf("writing script: %w", err)
	}

	// Write the systemd user service unit.
	// Goes to /etc/systemd/user/ so it's available for all users.
	unitDir := filepath.Join(etcDir, "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", unitDir, err)
	}

	unit := fmt.Sprintf(`[Unit]
Description=Install OEM-specific packages (first login only)
After=network-online.target
Wants=network-online.target
ConditionPathExists=!%%h/.config/%s/oem-setup-done

[Service]
Type=oneshot
ExecStart=/etc/%s/oem/install-packages.sh
RemainAfterExit=no

[Install]
WantedBy=default.target
`, distroID, distroID)

	unitPath := filepath.Join(unitDir, distroID+"-oem-setup.service")
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("writing unit: %w", err)
	}

	// Enable the service for all users via a preset file.
	presetDir := filepath.Join(unitDir + "-preset")
	if err := os.MkdirAll(presetDir, 0o755); err != nil {
		return fmt.Errorf("mkdir preset dir: %w", err)
	}
	preset := fmt.Sprintf("enable %s-oem-setup.service\n", distroID)
	presetPath := filepath.Join(presetDir, "50-"+distroID+"-oem.preset")
	if err := os.WriteFile(presetPath, []byte(preset), 0o644); err != nil {
		return fmt.Errorf("writing preset: %w", err)
	}

	return nil
}

// enableSystemService enables a systemd system service in the target.
func enableSystemService(target string, service string) {
	// Create a symlink in the target's multi-user.target.wants.
	var etcDir string
	if isComposeFsNative(target) {
		var err error
		etcDir, err = ComposeFsDeployEtcDirFn(target)
		if err != nil {
			progress.Info(fmt.Sprintf("Warning: could not enable %s (composefs deploy etc): %v", service, err))
			return
		}
	} else {
		deployDir, err := DeploymentDirFn(target)
		if err != nil {
			progress.Info(fmt.Sprintf("Warning: could not enable %s: %v", service, err))
			return
		}
		etcDir = filepath.Join(deployDir, "etc")
	}

	wantsDir := filepath.Join(etcDir, "systemd", "system", "multi-user.target.wants")
	if err := os.MkdirAll(wantsDir, 0o755); err != nil {
		progress.Info(fmt.Sprintf("Warning: could not create wants dir for %s: %v", service, err))
		return
	}

	// Symlink to the unit file in /usr/lib/systemd/system/
	link := filepath.Join(wantsDir, service)
	unitTarget := filepath.Join("/usr/lib/systemd/system", service)
	// Remove existing symlink if present
	os.Remove(link)
	if err := os.Symlink(unitTarget, link); err != nil {
		// Fallback: use runner to create the symlink (handles permission issues)
		if err2 := runner.Run("ln", "-sf", unitTarget, link); err2 != nil {
			progress.Info(fmt.Sprintf("Warning: could not enable %s: %v", service, err2))
		}
	}
}

// writeMenuIconOverride writes a dconf override that sets the custom-command-menu
// icon to the vendor's logo. This persists across reboots as a system dconf lock.
func writeMenuIconOverride(target string, iconName string) error {
	var etcDir string
	if isComposeFsNative(target) {
		var err error
		etcDir, err = ComposeFsDeployEtcDirFn(target)
		if err != nil {
			return fmt.Errorf("finding composefs deploy etc for dconf override: %w", err)
		}
	} else {
		deployDir, err := DeploymentDirFn(target)
		if err != nil {
			return fmt.Errorf("finding deployment dir: %w", err)
		}
		etcDir = filepath.Join(deployDir, "etc")
	} 
	// Write dconf local.d override (higher priority than system defaults).
	dconfDir := filepath.Join(etcDir, "dconf", "db", "local.d")
	if err := os.MkdirAll(dconfDir, 0o755); err != nil {
		return fmt.Errorf("mkdir dconf dir: %w", err)
	}

	override := fmt.Sprintf(`[org/gnome/shell/extensions/custom-command-list]
menuicon-setting='%s'
`, iconName)

	overridePath := filepath.Join(dconfDir, "02-oem-menu-icon")
	if err := os.WriteFile(overridePath, []byte(override), 0o644); err != nil {
		return fmt.Errorf("writing dconf override: %w", err)
	}

	return nil
}
