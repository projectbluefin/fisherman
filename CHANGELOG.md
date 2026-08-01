# Fisherman Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### 🐛 Bug Fixes

- **Scratch OCI cache freed before post-install (ENOSPC → "error writing
  hostname")**: On live ISOs the scratch dir holding several GB of extracted
  image blobs lives on the *target* disk. It was only deleted by the final
  cleanup pass, so every post-install write (Flatpak copy, hostname, fstab)
  competed with it for space. On modest disks (≤ ~25 GiB) the install failed at
  98% with the misleading `error writing hostname: write
  /mnt/fisherman-target/state/deploy/<hash>/etc/hostname`. `Cleanup.ReleaseScratch`
  now unmounts and deletes the cache immediately after `bootc install` returns.
  A failed early release keeps its cleanup registrations so final teardown still
  retries it — otherwise a busy unmount would leak the multi-GB cache onto the
  installed system permanently.
- **Full target disk fails loudly**: `tar` reports ENOSPC only on stderr, so the
  Flatpak copy step saw a bare `exit status 2`, downgraded it to a warning, and
  let the install limp on to fail later with an unrelated error. ENOSPC is now
  detected (`post.IsNoSpace`) and aborts the install with
  `target disk is full — /dev/… is too small for this image`.

- **OCI layout for non-composefs installs**: Non-composefs images (bluefin, lts,
  lts-hwe) now export to an OCI layout at scratch and use `--source-imgref oci:...`
  for `bootc install to-filesystem`. The previous VFS squash path corrupted ostree
  commit structure, producing “Expected commit object, not File”. The overlay
  redirect eliminates ENOSPC on 8+ GiB images by placing working container layers
  on the target disk instead of the 1.4 GiB live overlayfs.
- **`--source-imgref` in direct mode**: Direct mode now emits `--source-imgref`
  so `bootc install to-filesystem` does not fail with “must be executed inside
  a podman container” on offline live-ISO installs.

### 📊 Partition Layout

- **2 GiB /boot**: grub2 `/boot` ext4 partition increased from 1 GiB → 2 GiB.
  Each kernel+initramfs pair (200–400 MiB with NVIDIA drivers) fills 1 GiB quickly
  once deployment + rollback + staged upgrade accumulate.
- **2 GiB ESP for all fleet images**: grub2 EFI System Partition increased from
  512 MiB → 2 GiB for fleet consistency with systemd-boot (dakota) images.

---

## [0.2.0] - 2026-05-08

### 🚀 Major Features

#### Storage & Filesystem Support
- **Composefs Overlay Storage**: Full support for overlay storage driver with composefs installations
- **ext4 Root Filesystem**: Added ext4 with verity support for composefs backend
- **Improved Partition Layout**: Auto-detect and optimize storage driver selection

#### systemd-boot & EFI Improvements
- **EFI Fallback Installation**: Robust systemd-boot EFI binary installation with fallback paths
- **Dynamic BOOTX64.EFI Handling**: Search multiple ostree deployment paths for systemd-boot binary
- **GPT Retagging Fixes**: Proper EFI remount handling during partition retagging

#### Release Automation
- **GoReleaser Integration**: Added `.goreleaser.yml` for automated binary releases
- **Release Workflows**: New `release-cut.yml` and `release-publish.yml` workflows
- **Dependabot Updates**: Added `.github/dependabot.yml` for automated dependency checks
- **GolangCI Linting**: Added `.golangci.yml` for consistent code quality

### 🐛 Bug Fixes

#### SSH & Boot Verification
- Enable systemd-networkd DHCP in SSH test images
- Add SSH troubleshooting to boot-verify script
- Improve SSH enablement in Containerfile for debian-bootc

#### Composefs & Storage
- Fix systemd.wants=ssh.service on composefs boot parameters
- Redirect podman container storage to scratch for composefs installs
- Improve overlayfs /var scratch directory detection
- Enable overlay storage driver on btrfs (previously disabled)

#### QEMU & Testing
- Add explicit QEMU boot order for composefs VM testing
- Revert ineffective QEMU boot order and systemd-boot timeout fixes
- Improve boot configuration detection and patching logic

### 📊 Test Coverage
- Add loopback device storage driver tests
- Add LUKS unlocking tests with Python helper script
- Add post-install verification tests
- Expand SSH image building for all VM boot test images

### 🔨 Improvements
- Refactor test matrix with Dakota and improved boot verification
- Enhance script discovery for CI/local portability
- Add `scripts/luks-unlock.py` for LUKS testing automation
- Improve `verify-installation.sh` with comprehensive status checks

---

## [0.1.0] - 2026-03-XX

### Initial Release
- First stable release of fisherman bootc installer backend
- Support for bootc install with custom partitioning
- LUKS encryption support
- Composefs with systemd-boot support
- Image catalog and customization
- flatpak installation support
- Post-install hostname and user creation
