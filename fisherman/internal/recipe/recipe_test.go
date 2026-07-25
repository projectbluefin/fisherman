package recipe_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/fisherman/internal/recipe"
)

func TestValidate(t *testing.T) {
	// Create a real file to satisfy os.Stat in Validate.
	dir := t.TempDir()
	diskPath := filepath.Join(dir, "fake-disk")
	if err := os.WriteFile(diskPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		r       recipe.Recipe
		wantErr string // substring; empty means no error expected
	}{
		// ── Valid recipes ──────────────────────────────────────────────────────
		{
			name: "valid xfs",
			r:    recipe.Recipe{Disk: diskPath, Filesystem: "xfs", Hostname: "host1"},
		},
		{
			name: "valid btrfs",
			r:    recipe.Recipe{Disk: diskPath, Filesystem: "btrfs", Hostname: "host1"},
		},
		{
			name: "valid btrfs subvolumes",
			r:    recipe.Recipe{Disk: diskPath, Filesystem: "btrfs", BtrfsSubvolumes: true, Hostname: "host1"},
		},
		{
			name: "valid encryption none",
			r:    recipe.Recipe{Disk: diskPath, Filesystem: "xfs", Hostname: "h", Encryption: recipe.Encryption{Type: "none"}},
		},
		{
			name: "valid empty encryption type",
			r:    recipe.Recipe{Disk: diskPath, Filesystem: "xfs", Hostname: "h"},
		},
		{
			name: "valid luks-passphrase",
			r: recipe.Recipe{
				Disk: diskPath, Filesystem: "xfs", Hostname: "h",
				Encryption: recipe.Encryption{Type: "luks-passphrase", Passphrase: "secret"},
			},
		},
		{
			name: "valid tpm2-luks (no passphrase required)",
			r: recipe.Recipe{
				Disk: diskPath, Filesystem: "xfs", Hostname: "h",
				Encryption: recipe.Encryption{Type: "tpm2-luks"},
			},
		},
		{
			name: "valid tpm2-luks-passphrase",
			r: recipe.Recipe{
				Disk: diskPath, Filesystem: "xfs", Hostname: "h",
				Encryption: recipe.Encryption{Type: "tpm2-luks-passphrase", Passphrase: "secret"},
			},
		},
		{
			name: "valid composefs_backend true",
			r:    recipe.Recipe{Disk: diskPath, Filesystem: "btrfs", Hostname: "h", ComposeFsBackend: true},
		},
		{
			name: "valid composefs_backend with luks-passphrase",
			r: recipe.Recipe{
				Disk: diskPath, Filesystem: "btrfs", Hostname: "h",
				ComposeFsBackend: true,
				Encryption:       recipe.Encryption{Type: "luks-passphrase", Passphrase: "secret"},
			},
		},
		{
			name: "valid composefs_backend with btrfs",
			r:    recipe.Recipe{Disk: diskPath, Filesystem: "btrfs", Hostname: "h", ComposeFsBackend: true},
		},
		{
			name: "valid composefs_backend with xfs",
			r:    recipe.Recipe{Disk: diskPath, Filesystem: "xfs", Hostname: "h", ComposeFsBackend: true},
		},
		{
			name: "valid bootloader empty (default grub2)",
			r:    recipe.Recipe{Disk: diskPath, Filesystem: "xfs", Hostname: "h"},
		},
		{
			name: "valid bootloader grub2",
			r:    recipe.Recipe{Disk: diskPath, Filesystem: "btrfs", Hostname: "h", Bootloader: "grub2"},
		},
		{
			name: "valid bootloader systemd",
			r:    recipe.Recipe{Disk: diskPath, Filesystem: "btrfs", Hostname: "h", Bootloader: "systemd"},
		},

		// ── Invalid: disk ─────────────────────────────────────────────────────
		{
			name:    "empty disk",
			r:       recipe.Recipe{Filesystem: "xfs", Hostname: "h"},
			wantErr: "disk is required",
		},
		{
			name:    "nonexistent disk",
			r:       recipe.Recipe{Disk: "/dev/definitely-does-not-exist-xyzzy", Filesystem: "xfs", Hostname: "h"},
			wantErr: "disk /dev/definitely-does-not-exist-xyzzy",
		},

		// ── Invalid: filesystem ───────────────────────────────────────────────
		{
			name:    "empty filesystem",
			r:       recipe.Recipe{Disk: diskPath, Hostname: "h"},
			wantErr: `filesystem must be`,
		},
		{
			name: "ext4 filesystem valid",
			r:    recipe.Recipe{Disk: diskPath, Filesystem: "ext4", Hostname: "h"},
		},
		{
			name:    "btrfsSubvolumes without btrfs",
			r:       recipe.Recipe{Disk: diskPath, Filesystem: "xfs", BtrfsSubvolumes: true, Hostname: "h"},
			wantErr: "btrfsSubvolumes requires filesystem=btrfs",
		},
		// ── Invalid: imageType ────────────────────────────────────────────────
		{
			name:    "imageType ostree not yet supported",
			r:       recipe.Recipe{Disk: diskPath, Filesystem: "xfs", Hostname: "h", ImageType: "ostree"},
			wantErr: "imageType \"ostree\" is not yet supported",
		},
		{
			name:    "imageType unknown value",
			r:       recipe.Recipe{Disk: diskPath, Filesystem: "xfs", Hostname: "h", ImageType: "flatpak"},
			wantErr: "imageType must be",
		},

		// ── Invalid: bootloader ───────────────────────────────────────────────
		{
			name:    "bootloader unknown value",
			r:       recipe.Recipe{Disk: diskPath, Filesystem: "xfs", Hostname: "h", Bootloader: "lilo"},
			wantErr: "bootloader must be",
		},

		// ── Invalid: encryption ───────────────────────────────────────────────
		{
			name: "luks-passphrase empty passphrase",
			r: recipe.Recipe{
				Disk: diskPath, Filesystem: "xfs", Hostname: "h",
				Encryption: recipe.Encryption{Type: "luks-passphrase"},
			},
			wantErr: "passphrase required",
		},
		{
			name: "tpm2-luks-passphrase empty passphrase",
			r: recipe.Recipe{
				Disk: diskPath, Filesystem: "xfs", Hostname: "h",
				Encryption: recipe.Encryption{Type: "tpm2-luks-passphrase"},
			},
			wantErr: "passphrase required",
		},
		{
			name: "unknown encryption type",
			r: recipe.Recipe{
				Disk: diskPath, Filesystem: "xfs", Hostname: "h",
				Encryption: recipe.Encryption{Type: "invalid-type"},
			},
			wantErr: "encryption.type must be",
		},

		// ── Invalid: hostname ─────────────────────────────────────────────────
		{
			name:    "empty hostname",
			r:       recipe.Recipe{Disk: diskPath, Filesystem: "xfs"},
			wantErr: "hostname is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.r.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Errorf("Validate() expected error containing %q, got nil", tt.wantErr)
				} else if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("Validate() error = %q, want containing %q", err.Error(), tt.wantErr)
				}
			}
		})
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()

	t.Run("valid JSON", func(t *testing.T) {
		r := &recipe.Recipe{
			Disk:       "/dev/sda",
			Filesystem: "xfs",
			Hostname:   "myhost",
			Flatpaks:   []string{"org.mozilla.firefox"},
		}
		data, _ := json.Marshal(r)
		path := filepath.Join(dir, "recipe.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}

		loaded, err := recipe.Load(path)
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if loaded.Disk != "/dev/sda" {
			t.Errorf("Disk = %q, want /dev/sda", loaded.Disk)
		}
		if loaded.Filesystem != "xfs" {
			t.Errorf("Filesystem = %q, want xfs", loaded.Filesystem)
		}
		if loaded.Hostname != "myhost" {
			t.Errorf("Hostname = %q, want myhost", loaded.Hostname)
		}
		if len(loaded.Flatpaks) != 1 || loaded.Flatpaks[0] != "org.mozilla.firefox" {
			t.Errorf("Flatpaks = %v, want [org.mozilla.firefox]", loaded.Flatpaks)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := recipe.Load(filepath.Join(dir, "nonexistent.json"))
		if err == nil {
			t.Fatal("expected error for missing file")
		}
		if !strings.Contains(err.Error(), "reading recipe") {
			t.Errorf("error = %q, want containing 'reading recipe'", err.Error())
		}
	})

	t.Run("additional image stores + mount overrides round-trip", func(t *testing.T) {
		body := []byte(`{
            "disk": "/dev/sda",
            "filesystem": "xfs",
            "hostname": "h",
            "additionalImageStores": ["/var/lib/superiso-store", "/srv/extra"],
            "targetMount": "/mnt/altroot",
            "luksMapperName": "altmapper"
        }`)
		path := filepath.Join(dir, "stores.json")
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		loaded, err := recipe.Load(path)
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if got := loaded.AdditionalImageStores; len(got) != 2 ||
			got[0] != "/var/lib/superiso-store" || got[1] != "/srv/extra" {
			t.Errorf("AdditionalImageStores = %v, want [/var/lib/superiso-store /srv/extra]", got)
		}
		if loaded.TargetMount != "/mnt/altroot" {
			t.Errorf("TargetMount = %q, want /mnt/altroot", loaded.TargetMount)
		}
		if loaded.LuksMapperName != "altmapper" {
			t.Errorf("LuksMapperName = %q, want altmapper", loaded.LuksMapperName)
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		path := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := recipe.Load(path)
		if err == nil {
			t.Fatal("expected error for malformed JSON")
		}
		if !strings.Contains(err.Error(), "parsing recipe") {
			t.Errorf("error = %q, want containing 'parsing recipe'", err.Error())
		}
	})
}

// Manual (customMounts) layouts: two ways a recipe can be accepted here and
// then do the wrong thing later. Both were hit in practice by
// tuna-os/bootc-installer-asahi.

func manualRecipe(t *testing.T, fstype string, enc string) *recipe.Recipe {
	t.Helper()
	// Validate() stats the partition paths, so use files that exist.
	root := filepath.Join(t.TempDir(), "root")
	if err := os.WriteFile(root, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	r := &recipe.Recipe{
		Image:        "example.invalid/img:latest",
		Hostname:     "validate-test",
		CustomMounts: []recipe.CustomMount{{Partition: root, Target: "/", Fstype: fstype}},
	}
	r.Encryption.Type = enc
	return r
}

func TestValidateRejectsUnsupportedCustomMountFstype(t *testing.T) {
	// "vfat" is the obvious spelling for an ESP and is NOT accepted:
	// formatPartition knows "fat32". Previously this passed Validate() and
	// failed mid-install, after the caller had already committed to the recipe.
	err := manualRecipe(t, "vfat", "none").Validate()
	if err == nil {
		t.Fatal("expected an unsupported-fstype error, got nil")
	}
	if !strings.Contains(err.Error(), "vfat") {
		t.Errorf("error should name the offending value, got: %v", err)
	}
}

func TestValidateAcceptsSkipFormatSentinels(t *testing.T) {
	// An existing ESP must be mountable WITHOUT being reformatted: it already
	// holds the bootloader and, on Apple Silicon, non-redistributable vendor
	// firmware. Both spellings must survive validation.
	for _, fstype := range []string{"", "unformatted"} {
		if err := manualRecipe(t, fstype, "none").Validate(); err != nil {
			t.Errorf("fstype %q should be accepted, got: %v", fstype, err)
		}
	}
}

func TestValidateAcceptsSupportedCustomMountFstypes(t *testing.T) {
	for _, fstype := range []string{"fat32", "ext3", "ext4", "xfs", "btrfs"} {
		if err := manualRecipe(t, fstype, "none").Validate(); err != nil {
			t.Errorf("fstype %q should be accepted, got: %v", fstype, err)
		}
	}
}

func TestValidateRejectsEncryptionWithCustomMounts(t *testing.T) {
	// The manual path never runs luksFormat, so an encrypted manual recipe
	// installs UNENCRYPTED while the caller believes otherwise. Fail closed.
	for _, enc := range []string{"luks-passphrase", "tpm2-luks"} {
		err := manualRecipe(t, "xfs", enc).Validate()
		if err == nil {
			t.Fatalf("encryption %q with customMounts must be rejected", enc)
		}
		if !strings.Contains(err.Error(), "unencrypted") {
			t.Errorf("error should explain the consequence, got: %v", err)
		}
	}
}

func TestValidateAllowsNoEncryptionWithCustomMounts(t *testing.T) {
	for _, enc := range []string{"", "none"} {
		if err := manualRecipe(t, "xfs", enc).Validate(); err != nil {
			t.Errorf("encryption %q should be accepted, got: %v", enc, err)
		}
	}
}
