// Package slurp implements pre-partition data extraction from existing OS
// partitions. Currently supports extracting Windows wallpapers from NTFS.
package slurp

import (
	"crypto/md5"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tuna-os/fisherman/internal/progress"
	"github.com/tuna-os/fisherman/internal/runner"
)

const (
	// scratchBase is the RAM-backed directory where slurped data is held
	// between extraction (pre-partition) and injection (post-install).
	// /run is tmpfs — survives disk wipe, freed on reboot.
	scratchBase = "/run/fisherman-slurp"

	// ntfsMountPoint is where we temporarily mount the Windows partition.
	ntfsMountPoint = "/run/fisherman-slurp-mnt"
)

// WallpaperResult holds the outcome of a wallpaper extraction attempt.
type WallpaperResult struct {
	// Found is true if at least one wallpaper was extracted.
	Found bool
	// ScratchDir is the path where wallpapers are stored (under scratchBase).
	ScratchDir string
	// Count is the number of wallpaper files extracted.
	Count int
	// TotalBytes is the total size of extracted wallpapers.
	TotalBytes int64
}

// windowsWallpaperPaths are the known locations where Windows stores wallpapers,
// relative to a user profile directory (e.g. C:\Users\Jorge\).
var windowsWallpaperPaths = []string{
	// Current wallpaper (transcoded copy Windows keeps active)
	"AppData/Roaming/Microsoft/Windows/Themes/TranscodedWallpaper",
	// Windows Spotlight lock screen / desktop images
	"AppData/Local/Packages/Microsoft.Windows.ContentDeliveryManager_cw5n1h2txyewy/LocalState/Assets",
	// User-set wallpapers cached by Windows
	"AppData/Roaming/Microsoft/Windows/Themes/CachedFiles",
}

// commonWallpaperExtensions filters files to only image types.
var commonWallpaperExtensions = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".bmp": true,
	".webp": true, ".tiff": true, ".tif": true,
}

// DetectNTFS finds NTFS partitions on the given disk device.
// Returns partition device paths (e.g. ["/dev/nvme0n1p3"]).
func DetectNTFS(disk string) ([]string, error) {
	out, err := runner.Output("lsblk", "-nrpo", "NAME,FSTYPE", disk)
	if err != nil {
		return nil, fmt.Errorf("lsblk: %w", err)
	}

	var ntfsPartitions []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == "ntfs" {
			ntfsPartitions = append(ntfsPartitions, fields[0])
		}
	}
	return ntfsPartitions, nil
}

// ExtractWallpapers mounts an NTFS partition read-only, finds Windows user
// wallpapers, and copies them to RAM-backed scratch storage.
// This must be called BEFORE partitioning destroys the source data.
// Returns a result describing what was found; errors are non-fatal.
func ExtractWallpapers(ntfsPartition string) (*WallpaperResult, error) {
	result := &WallpaperResult{}

	// Create scratch and mount directories
	if err := os.MkdirAll(scratchBase, 0o755); err != nil {
		return result, fmt.Errorf("creating scratch dir: %w", err)
	}
	if err := os.MkdirAll(ntfsMountPoint, 0o755); err != nil {
		return result, fmt.Errorf("creating mount point: %w", err)
	}

	// Mount NTFS read-only
	progress.Substep("Mounting Windows partition (read-only)")
	if err := runner.Run("mount", "-t", "ntfs3", "-o", "ro,noatime", ntfsPartition, ntfsMountPoint); err != nil {
		// Fallback to ntfs-3g if kernel ntfs3 driver not available
		if err2 := runner.Run("mount", "-t", "ntfs-3g", "-o", "ro,noatime", ntfsPartition, ntfsMountPoint); err2 != nil {
			os.Remove(ntfsMountPoint)
			return result, fmt.Errorf("mounting NTFS (%s): ntfs3: %v, ntfs-3g: %v", ntfsPartition, err, err2)
		}
	}
	// Always unmount when done
	defer func() {
		_ = runner.Run("umount", ntfsMountPoint)
		os.Remove(ntfsMountPoint)
	}()

	// Find Windows user profiles
	usersDir := filepath.Join(ntfsMountPoint, "Users")
	entries, err := os.ReadDir(usersDir)
	if err != nil {
		return result, fmt.Errorf("reading Users directory: %w", err)
	}

	wallpaperDir := filepath.Join(scratchBase, "wallpapers")
	if err := os.MkdirAll(wallpaperDir, 0o755); err != nil {
		return result, fmt.Errorf("creating wallpaper scratch: %w", err)
	}

	// Skip system profiles
	skipProfiles := map[string]bool{
		"Public": true, "Default": true, "Default User": true,
		"All Users": true, "desktop.ini": true,
	}

	for _, entry := range entries {
		if !entry.IsDir() || skipProfiles[entry.Name()] || strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		profileDir := filepath.Join(usersDir, entry.Name())
		userWallpaperDir := filepath.Join(wallpaperDir, entry.Name())

		extracted := extractFromProfile(profileDir, userWallpaperDir)
		result.Count += extracted
	}

	// Also check the system-wide wallpaper directory
	sysWallpaperDir := filepath.Join(ntfsMountPoint, "Windows", "Web", "Wallpaper")
	if info, err := os.Stat(sysWallpaperDir); err == nil && info.IsDir() {
		sysTarget := filepath.Join(wallpaperDir, "_windows_defaults")
		extracted := extractFromDir(sysWallpaperDir, sysTarget, 10) // cap at 10 system wallpapers
		result.Count += extracted
	}

	if result.Count > 0 {
		result.Found = true
		result.ScratchDir = wallpaperDir
		result.TotalBytes = dirSize(wallpaperDir)
		progress.Substep(fmt.Sprintf("Extracted %d wallpaper(s) (%s)", result.Count, humanBytes(result.TotalBytes)))
	}

	return result, nil
}

// extractFromProfile extracts wallpapers from a single Windows user profile.
func extractFromProfile(profileDir, targetDir string) int {
	count := 0

	for _, relPath := range windowsWallpaperPaths {
		srcPath := filepath.Join(profileDir, relPath)
		info, err := os.Stat(srcPath)
		if err != nil {
			continue
		}

		if info.IsDir() {
			// Directory of wallpapers (e.g. Spotlight assets, CachedFiles)
			count += extractFromDir(srcPath, targetDir, 20)
		} else {
			// Single file (e.g. TranscodedWallpaper)
			if info.Size() > 10*1024 { // skip tiny files (<10KB, likely not wallpapers)
				if err := copyFile(srcPath, targetDir, info.Name()); err == nil {
					count++
				}
			}
		}
	}

	// Also check Pictures folder for common wallpaper-named files
	picturesDir := filepath.Join(profileDir, "Pictures")
	if info, err := os.Stat(picturesDir); err == nil && info.IsDir() {
		count += extractWallpapersFromPictures(picturesDir, targetDir)
	}

	return count
}

// extractFromDir copies image files from a source directory to a target,
// limited to maxFiles. Only copies files that look like wallpapers (by size
// and extension). Returns count of files copied.
func extractFromDir(srcDir, targetDir string, maxFiles int) int {
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return 0
	}

	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return 0
	}

	count := 0
	for _, entry := range entries {
		if count >= maxFiles {
			break
		}
		if entry.IsDir() {
			continue
		}

		info, err := entry.Info()
		if err != nil || info.Size() < 50*1024 { // wallpapers are usually >50KB
			continue
		}
		// Cap at 20MB per file to avoid pulling huge BMPs
		if info.Size() > 20*1024*1024 {
			continue
		}

		name := entry.Name()
		ext := strings.ToLower(filepath.Ext(name))

		// Spotlight assets have no extension — check if plausible wallpaper size
		if ext == "" && info.Size() > 100*1024 && info.Size() < 20*1024*1024 {
			// Spotlight images are JPEGs without extension; rename with .jpg
			if err := copyFile(filepath.Join(srcDir, name), targetDir, name+".jpg"); err == nil {
				count++
			}
			continue
		}

		if !commonWallpaperExtensions[ext] {
			continue
		}

		if err := copyFile(filepath.Join(srcDir, name), targetDir, name); err == nil {
			count++
		}
	}
	return count
}

// extractWallpapersFromPictures looks for wallpaper-related images in the
// user's Pictures folder. Only grabs files in a "Wallpapers" or "Backgrounds"
// subfolder, or files with "wallpaper"/"background" in the name.
func extractWallpapersFromPictures(picturesDir, targetDir string) int {
	count := 0

	// Check common wallpaper subdirectories
	wallpaperSubdirs := []string{"Wallpapers", "Backgrounds", "wallpapers", "backgrounds"}
	for _, sub := range wallpaperSubdirs {
		subDir := filepath.Join(picturesDir, sub)
		if info, err := os.Stat(subDir); err == nil && info.IsDir() {
			count += extractFromDir(subDir, targetDir, 10)
		}
	}

	return count
}

// copyFile copies a single file from src to targetDir/name.
func copyFile(src, targetDir, name string) error {
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return err
	}

	dst := filepath.Join(targetDir, name)
	// Avoid overwriting (different profiles may have same-named files)
	if _, err := os.Stat(dst); err == nil {
		dst = filepath.Join(targetDir, "dup_"+name)
	}

	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

// InjectWallpapers copies extracted wallpapers from scratch into the installed
// system's user home directory. Sets up the backgrounds directory that GNOME
// discovers automatically, then pre-generates thumbnails so the wallpaper
// capplet doesn't stutter on first open.
func InjectWallpapers(target string, slurpResult *WallpaperResult, composeFsNative bool) error {
	if slurpResult == nil || !slurpResult.Found {
		return nil
	}

	// Determine the target home directory path
	var homeBase string
	if composeFsNative {
		homeBase = filepath.Join(target, "state", "os", "default", "var", "home")
	} else {
		homeBase = filepath.Join(target, "var", "home")
	}

	// Place wallpapers in a shared location that all users can access
	bgDir := filepath.Join(homeBase, ".windows-wallpapers")
	if err := os.MkdirAll(bgDir, 0o755); err != nil {
		return fmt.Errorf("creating wallpaper target dir: %w", err)
	}

	// Copy from scratch to target
	if err := runner.Run("cp", "-a", slurpResult.ScratchDir+"/.", bgDir+"/"); err != nil {
		return fmt.Errorf("copying wallpapers to target: %w", err)
	}

	// Set ownership to UID/GID 1000 (first user)
	if err := runner.Run("chown", "-R", "1000:1000", bgDir); err != nil {
		fmt.Fprintf(os.Stderr, "  warning: could not chown wallpapers: %v\n", err)
	}

	// Fix SELinux labels
	_ = runner.Run("restorecon", "-R", bgDir)

	progress.Info(fmt.Sprintf("Migrated %d Windows wallpaper(s) to %s", slurpResult.Count, bgDir))
	return nil
}

// GenerateSystemThumbnails pre-generates thumbnails for ALL wallpapers on the
// installed system — both system-provided (/usr/share/backgrounds/) and any
// user-injected ones. Call this after bootc install + wallpaper injection so
// the wallpaper capplet opens instantly on first boot.
func GenerateSystemThumbnails(target string, composeFsNative bool) int {
	thumbnailer := detectThumbnailer()
	if thumbnailer == "" {
		return 0
	}

	// Determine the user's home and cache paths on the target
	var homeBase string
	if composeFsNative {
		homeBase = filepath.Join(target, "state", "os", "default", "var", "home")
	} else {
		homeBase = filepath.Join(target, "var", "home")
	}
	cacheDir := filepath.Join(homeBase, ".cache", "thumbnails", "large")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return 0
	}

	// Directories to scan for wallpapers (on the mounted target filesystem)
	scanDirs := []string{
		filepath.Join(target, "usr", "share", "backgrounds"),
		filepath.Join(target, "usr", "share", "wallpapers"),
		filepath.Join(homeBase, ".windows-wallpapers"),
	}

	count := 0
	for _, dir := range scanDirs {
		if _, err := os.Stat(dir); err != nil {
			continue
		}

		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil //nolint:nilerr // skip this entry and keep walking; a partial scan is intended
			}
			// Skip tiny files and non-images
			if info.Size() < 10*1024 {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(path))
			if !commonWallpaperExtensions[ext] {
				return nil
			}

			// Compute the real path as it will exist on the installed system
			var installedPath string
			relToTarget, err := filepath.Rel(target, path)
			if err != nil {
				return nil //nolint:nilerr // skip this entry and keep walking; a partial scan is intended
			}
			installedPath = "/" + relToTarget

			fileURI := "file://" + installedPath
			thumbName := md5hex(fileURI) + ".png"
			thumbPath := filepath.Join(cacheDir, thumbName)

			// Skip if thumbnail already exists
			if _, err := os.Stat(thumbPath); err == nil {
				return nil
			}

			if generateSingleThumbnail(thumbnailer, path, thumbPath) {
				count++
			}
			return nil
		})
	}

	// Fix ownership on the entire thumbnail cache
	if count > 0 {
		thumbBase := filepath.Join(homeBase, ".cache", "thumbnails")
		_ = runner.Run("chown", "-R", "1000:1000", thumbBase)
		_ = runner.Run("restorecon", "-R", thumbBase)
	}

	return count
}

// detectThumbnailer returns the command to use for thumbnail generation.
func detectThumbnailer() string {
	// Prefer gdk-pixbuf-thumbnailer — it writes proper PNG metadata
	if _, err := runner.Output("which", "gdk-pixbuf-thumbnailer"); err == nil {
		return "gdk-pixbuf-thumbnailer"
	}
	// Fallback: ffmpeg (likely available on installer ISOs)
	if _, err := runner.Output("which", "ffmpeg"); err == nil {
		return "ffmpeg"
	}
	// Last resort: convert (ImageMagick)
	if _, err := runner.Output("which", "convert"); err == nil {
		return "convert"
	}
	return ""
}

// generateSingleThumbnail creates a 256x256 PNG thumbnail of src at dst.
func generateSingleThumbnail(thumbnailer, src, dst string) bool {
	var err error
	switch thumbnailer {
	case "gdk-pixbuf-thumbnailer":
		// -s 256 generates a 256px thumbnail
		err = runner.Run("gdk-pixbuf-thumbnailer", "-s", "256", src, dst)
	case "ffmpeg":
		// Scale to fit 256x256, maintaining aspect ratio
		err = runner.Run("ffmpeg", "-y", "-i", src,
			"-vf", "scale=256:256:force_original_aspect_ratio=decrease",
			"-frames:v", "1", "-f", "image2", dst)
	case "convert":
		err = runner.Run("convert", src, "-thumbnail", "256x256>", "-strip", dst)
	}
	return err == nil
}

// md5hex returns the hex-encoded MD5 hash of s.
func md5hex(s string) string {
	h := md5.New()
	h.Write([]byte(s))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// CleanupScratch removes the RAM-backed scratch directory.
func CleanupScratch() {
	os.RemoveAll(scratchBase)
}

// dirSize returns total bytes of all files under a directory.
func dirSize(path string) int64 {
	var total int64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil //nolint:nilerr // skip this entry and keep walking; a partial scan is intended
		}
		total += info.Size()
		return nil
	})
	return total
}

func humanBytes(b int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)
	switch {
	case b >= GB:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(GB))
	case b >= MB:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(MB))
	case b >= KB:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(KB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
