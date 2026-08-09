package slurp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tuna-os/fisherman/internal/runner"
)

// ScanResult describes the Windows data available for migration on a disk.
// Emitted as JSON by `fisherman scan <disk>` for the GUI to display.
type ScanResult struct {
	Disk       string         `json:"disk"`
	Partitions []NTFSPartScan `json:"partitions"`
}

// NTFSPartScan describes a single NTFS partition's available user data.
type NTFSPartScan struct {
	Partition  string     `json:"partition"`
	Users      []UserScan `json:"users"`
	TotalBytes int64      `json:"totalBytes"`
}

// UserScan describes one Windows user profile's available data.
type UserScan struct {
	Name       string         `json:"name"`
	Categories []CategoryScan `json:"categories"`
	TotalBytes int64          `json:"totalBytes"`
}

// CategoryScan describes one data category (Documents, Pictures, etc.)
type CategoryScan struct {
	Name  string `json:"name"`
	Path  string `json:"path"` // relative to user profile
	Bytes int64  `json:"bytes"`
	Count int    `json:"count"` // number of files
}

// userDataCategories maps human-readable category names to their Windows paths
// relative to a user profile directory.
var userDataCategories = []struct {
	Name string
	Path string
}{
	{"Documents", "Documents"},
	{"Desktop", "Desktop"},
	{"Downloads", "Downloads"},
	{"Pictures", "Pictures"},
	{"Music", "Music"},
	{"Videos", "Videos"},
	{"Wallpapers", "AppData/Roaming/Microsoft/Windows/Themes"},
}

// Scan mounts the NTFS partitions on a disk read-only and enumerates available
// user data for migration. Returns a ScanResult suitable for JSON serialization
// to the GUI. Does NOT extract any data — just measures what's available.
func Scan(disk string) (*ScanResult, error) {
	result := &ScanResult{Disk: disk}

	ntfsPartitions, err := DetectNTFS(disk)
	if err != nil {
		return result, err
	}
	if len(ntfsPartitions) == 0 {
		return result, nil // no NTFS found — not an error
	}

	mountPoint := "/run/fisherman-slurp-scan"
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return result, fmt.Errorf("creating scan mount point: %w", err)
	}
	defer os.Remove(mountPoint)

	for _, part := range ntfsPartitions {
		partScan, err := scanPartition(part, mountPoint)
		if err != nil {
			continue // skip unreadable partitions
		}
		if len(partScan.Users) > 0 {
			result.Partitions = append(result.Partitions, *partScan)
		}
	}

	return result, nil
}

// scanPartition mounts one NTFS partition and enumerates its user data.
func scanPartition(partition, mountPoint string) (*NTFSPartScan, error) {
	// Mount read-only
	if err := runner.Run("mount", "-t", "ntfs3", "-o", "ro,noatime", partition, mountPoint); err != nil {
		if err2 := runner.Run("mount", "-t", "ntfs-3g", "-o", "ro,noatime", partition, mountPoint); err2 != nil {
			return nil, fmt.Errorf("mount failed")
		}
	}
	defer func() {
		_ = runner.Run("umount", mountPoint)
	}()

	result := &NTFSPartScan{Partition: partition}

	usersDir := filepath.Join(mountPoint, "Users")
	entries, err := os.ReadDir(usersDir)
	if err != nil {
		return nil, fmt.Errorf("no Users directory")
	}

	skipProfiles := map[string]bool{
		"Public": true, "Default": true, "Default User": true,
		"All Users": true, "desktop.ini": true,
	}

	for _, entry := range entries {
		if !entry.IsDir() || skipProfiles[entry.Name()] || strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		profileDir := filepath.Join(usersDir, entry.Name())
		userScan := scanUserProfile(entry.Name(), profileDir)
		if userScan.TotalBytes > 0 {
			result.Users = append(result.Users, *userScan)
			result.TotalBytes += userScan.TotalBytes
		}
	}

	return result, nil
}

// scanUserProfile measures data in each category for one Windows user.
func scanUserProfile(name, profileDir string) *UserScan {
	user := &UserScan{Name: name}

	for _, cat := range userDataCategories {
		catDir := filepath.Join(profileDir, cat.Path)
		info, err := os.Stat(catDir)
		if err != nil || !info.IsDir() {
			continue
		}

		var bytes int64
		var count int
		_ = filepath.Walk(catDir, func(_ string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() {
				return nil //nolint:nilerr // skip this entry and keep walking; a partial scan is intended
			}
			bytes += fi.Size()
			count++
			return nil
		})

		if bytes > 0 {
			user.Categories = append(user.Categories, CategoryScan{
				Name:  cat.Name,
				Path:  cat.Path,
				Bytes: bytes,
				Count: count,
			})
			user.TotalBytes += bytes
		}
	}

	return user
}

// ScanJSON runs Scan and returns the result as a JSON string (for CLI output).
func ScanJSON(disk string) (string, error) {
	result, err := Scan(disk)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}
