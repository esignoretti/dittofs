#!/usr/bin/env bash
#
# fetch-artifacts.sh — build the kernel + initramfs that boot-qemu.sh needs.
#
# boot-qemu.sh boots a REAL Linux kernel whose root filesystem is a DittoFS NBD
# export. Nothing in this repository builds such a kernel, so the artifacts have
# to come from somewhere. This script fetches them from Alpine's package mirrors
# and assembles an initramfs source tree that boot-qemu.sh can pack.
#
#   ./test/nbd/fetch-artifacts.sh                  # fetch into test/nbd/artifacts/
#   ./test/nbd/fetch-artifacts.sh --dest /tmp/nbd  # somewhere else
#   ./test/nbd/fetch-artifacts.sh --force          # re-fetch even if present
#
# Then:
#   ./test/nbd/boot-qemu.sh --kernel <dest>/Image \
#       --initramfs <dest>/initramfs.cpio.gz --initramfs-dir <dest>/initramfs \
#       --volume-path <your share mount> --export <share>/<volume>
#
# ---------------------------------------------------------------------------
# WHY THESE PACKAGES, AND NOT A READYMADE IMAGE
# ---------------------------------------------------------------------------
#
# Alpine's netboot initramfs (releases/aarch64/netboot/initramfs-virt) looks like
# the obvious shortcut and does not work: its module tree and the kernel shipped
# beside it are a DIFFERENT build from the one in the main repository. Measured,
# the v3.20 netboot kernel is 6.6.134 while the linux-virt package is 6.6.142, and
# an insmod of the mismatched nbd.ko fails on vermagic. The netboot initramfs also
# carries no nbd.ko at all. So this script takes the kernel AND its modules from
# the SAME package (linux-virt), which is the only combination whose vermagic is
# guaranteed to agree.
#
# The modules are extracted at their REAL nested paths
# (lib/modules/<ver>/kernel/<dir>/<mod>.ko.gz) rather than flattened, because that
# is the layout a distribution tree actually has and the layout the guest's module
# lookup must handle. Flattening them would hide a whole class of bug: an earlier
# version of boot-qemu.sh looked for /lib/modules/*/kernel/*/<mod>.ko but not the
# compressed form, and every module living at that depth (crc16, libcrc32c,
# crc32c_generic, mbcache) was silently NOT FOUND — invisible against a flattened
# tree, fatal against a real one.
#
# ---------------------------------------------------------------------------
# WHAT EACH PIECE IS FOR
# ---------------------------------------------------------------------------
#
#   linux-virt      the kernel (Image) and its modules: nbd, ext4, jbd2, mbcache,
#                   crc16, crc32c_generic, virtio_net and its failover chain
#   busybox         the guest's shell and every applet the init uses, including
#                   nbd-client, ip, insmod, mount, switch_root, gunzip
#   e2fsprogs       mke2fs, needed ONLY by a --mkfs boot (the first one)
#   e2fsprogs-libs  \
#   libcom_err       |  mke2fs's shared libraries
#   libblkid         |
#   libuuid          |
#   libeconf        /
#   musl            the dynamic loader mke2fs needs (the busybox is static)
#
# Versions are resolved from APKINDEX at fetch time rather than pinned, so this
# does not rot when Alpine publishes a point release. Alpine's busybox is a
# statically linked build with the nbd-client applet compiled in, which is why no
# separate nbd-client package is fetched — and why the guest needs no shared
# libraries at all unless it mkfs's.
#
# NOTE: the busybox nbd-client applet is fine for boot-qemu.sh, which only needs a
# handshake. It is NOT usable for crash-restart.sh, which needs a master-built
# nbd-client (no released build can reconnect). See test/nbd/README.md.

set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

# Alpine release. Pinned rather than "latest-stable" so a run today and a run next
# month fetch the same kernel; override if this release is ever withdrawn.
alpine_release=${ALPINE_RELEASE:-v3.20}
mirror=${ALPINE_MIRROR:-https://dl-cdn.alpinelinux.org/alpine}
arch=${ALPINE_ARCH:-aarch64}

dest=${NBD_ARTIFACT_DIR:-$script_dir/artifacts}
force=0

usage() {
	cat <<EOF
fetch-artifacts.sh — fetch the kernel and initramfs that boot-qemu.sh needs.

Usage:
  test/nbd/fetch-artifacts.sh [options]

Options:
  --dest DIR     Where to put the artifacts (default: test/nbd/artifacts,
                 or \$NBD_ARTIFACT_DIR). Created if missing.
  --force        Re-fetch even if the artifacts already exist.
  --release REL  Alpine release (default: \$ALPINE_RELEASE, else $alpine_release).
  --mirror URL   Alpine mirror (default: \$ALPINE_MIRROR).
  -h, --help     This text.

Environment:
  ALPINE_RELEASE, ALPINE_MIRROR, ALPINE_ARCH, NBD_ARTIFACT_DIR

Produces, under --dest:
  Image                  the kernel to pass as --kernel
  initramfs/             the source tree to pass as --initramfs-dir
  initramfs.cpio.gz      a packed copy, for --initramfs

Requires: curl (or wget), tar, gzip. cpio is NOT needed here — boot-qemu.sh
packs the archive, and it checks for cpio itself.
EOF
}

die() {
	printf 'fetch-artifacts.sh: REFUSING: %s\n' "$*" >&2
	exit 1
}

while [ $# -gt 0 ]; do
	case "$1" in
	--dest)
		[ $# -ge 2 ] || die "--dest needs a directory"
		dest=$2
		shift 2
		;;
	--force)
		force=1
		shift
		;;
	--release)
		[ $# -ge 2 ] || die "--release needs a release name (e.g. v3.20)"
		alpine_release=$2
		shift 2
		;;
	--mirror)
		[ $# -ge 2 ] || die "--mirror needs a URL"
		mirror=$2
		shift 2
		;;
	-h | --help)
		usage
		exit 0
		;;
	*) die "unknown argument '$1'. Use --help." ;;
	esac
done

# ---------------------------------------------------------------- prerequisites

# curl or wget. Both are checked because a minimal host often has only one, and a
# missing downloader is the first thing that would stop a first-time user.
fetch() {
	# fetch URL OUTFILE — a non-zero exit means the download failed, so callers
	# can name the URL rather than dying on a truncated file later.
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --max-time 300 -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -T 300 -O "$2" "$1"
	else
		die "neither curl nor wget is installed; one is required to fetch the artifacts"
	fi
}

for tool in tar gzip; do
	command -v "$tool" >/dev/null 2>&1 ||
		die "$tool not found; it is required to unpack the Alpine packages"
done

# ---------------------------------------------------------------- already there?

image=$dest/Image
initramfs_dir=$dest/initramfs
if [ "$force" != 1 ] && [ -r "$image" ] && [ -d "$initramfs_dir/bin" ]; then
	printf 'fetch-artifacts.sh: %s already exists (use --force to re-fetch)\n' "$image"
	printf '  kernel     : %s\n' "$image"
	printf '  initramfs  : %s\n' "$initramfs_dir"
	printf '\nNext:\n  test/nbd/boot-qemu.sh --kernel %s \\\n' "$image"
	printf '      --initramfs-dir %s --volume-path <share mount> --export <share>/<volume>\n' "$initramfs_dir"
	exit 0
fi

mkdir -p "$dest"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# ---------------------------------------------------------------- resolve versions

# APKINDEX is a blank-line-separated list of records; P: is the package name and
# the following V: is its version. Resolving beats pinning: the pinned versions in
# a URL go stale on every Alpine point release and the failure is a bare 404 that
# names nothing.
index_url=$mirror/$alpine_release/main/$arch/APKINDEX.tar.gz
printf 'fetch-artifacts.sh: resolving package versions from %s\n' "$index_url"
fetch "$index_url" "$work/APKINDEX.tar.gz" ||
	die "could not download $index_url. Check the release name ($alpine_release), the architecture ($arch), and your network."
tar -xzf "$work/APKINDEX.tar.gz" -C "$work" 2>/dev/null ||
	die "could not unpack $index_url (not a valid APKINDEX tarball?)"

resolve() {
	# resolve PKGNAME -> version, or empty. awk rather than grep -A so the record
	# boundary is respected: a P: line's version is the NEXT V: line, and grepping
	# for V: nearby can pick up a neighbouring package's.
	awk -v pkg="$1" '
		$0 == "P:" pkg { found = 1; next }
		found && /^V:/ { sub(/^V:/, ""); print; exit }
	' "$work/APKINDEX"
}

pkg_version() {
	v=$(resolve "$1")
	[ -n "$v" ] || die "package '$1' is not in $index_url. The release may have been withdrawn; try --release v3.21."
	printf '%s' "$v"
}

# linux-virt carries the kernel AND the modules. Fetching them from one package is
# what guarantees their vermagic agrees.
linux_virt_v=$(pkg_version linux-virt)
busybox_v=$(pkg_version busybox)
e2fsprogs_v=$(pkg_version e2fsprogs)
e2fsprogs_libs_v=$(pkg_version e2fsprogs-libs)
libcom_err_v=$(pkg_version libcom_err)
libblkid_v=$(pkg_version libblkid)
libuuid_v=$(pkg_version libuuid)
libeconf_v=$(pkg_version libeconf)
musl_v=$(pkg_version musl)

printf '  linux-virt      %s\n' "$linux_virt_v"
printf '  busybox         %s\n' "$busybox_v"
printf '  e2fsprogs       %s\n' "$e2fsprogs_v"
printf '  musl            %s\n' "$musl_v"

get_pkg() {
	# get_pkg NAME VERSION -> unpacks into $work/pkgs/NAME
	url=$mirror/$alpine_release/main/$arch/$1-$2.apk
	mkdir -p "$work/pkgs/$1"
	fetch "$url" "$work/$1.apk" ||
		die "could not download $url"
	tar -xzf "$work/$1.apk" -C "$work/pkgs/$1" 2>/dev/null ||
		die "could not unpack $work/$1.apk"
}

printf 'fetch-artifacts.sh: fetching packages\n'
for spec in \
	"linux-virt $linux_virt_v" \
	"busybox $busybox_v" \
	"e2fsprogs $e2fsprogs_v" \
	"e2fsprogs-libs $e2fsprogs_libs_v" \
	"libcom_err $libcom_err_v" \
	"libblkid $libblkid_v" \
	"libuuid $libuuid_v" \
	"libeconf $libeconf_v" \
	"musl $musl_v"; do
	# shellcheck disable=SC2086  # the split is the point: NAME VERSION
	get_pkg $spec
done

# ---------------------------------------------------------------- the kernel

kernel_src=$work/pkgs/linux-virt/boot/vmlinuz-virt
[ -r "$kernel_src" ] ||
	die "linux-virt $linux_virt_v has no boot/vmlinuz-virt. The package layout changed; inspect $work/pkgs/linux-virt."

# Confirm the kernel is the architecture we think it is. Alpine's vmlinuz-virt is
# an EFI-STUB kernel — a PE32+ executable with "MZ" at offset 0, NOT a raw arm64
# Image — and QEMU -kernel accepts it (that is how the recorded boots ran). The
# obvious check for the arm64 boot magic at 0x38 is therefore WRONG here: measured
# against the real file, bytes 0x38-0x3c are cd238281, so a magic check warns
# "QEMU may refuse it" about the one kernel that demonstrably works. Read the PE
# machine type instead, which is what actually distinguishes the architectures.
machine=$(python3 -c '
import struct, sys
d = open(sys.argv[1], "rb").read(0x100)
if d[0:2] != b"MZ":
    print("not-pe")
else:
    e = struct.unpack("<I", d[0x3c:0x40])[0]
    if d[e:e+4] != b"PE\0\0":
        print("not-pe")
    else:
        print(hex(struct.unpack("<H", d[e+4:e+6])[0]))
' "$kernel_src" 2>/dev/null || echo unknown)
if [ "$machine" = "not-pe" ]; then
	# A raw arm64 Image has the boot magic at 0x38. Accept it too, so this works if
	# Alpine ever ships one.
	magic=$(python3 -c '
import struct, sys
d = open(sys.argv[1], "rb").read(0x40)
print(struct.unpack("<I", d[0x38:0x3c])[0])
' "$kernel_src" 2>/dev/null || echo 0)
	if [ "$magic" != "1684301121" ]; then # 0x644d5241 = "ARMd"
		printf 'fetch-artifacts.sh: WARNING: %s is neither a PE (EFI-stub) kernel nor\n' "$kernel_src" >&2
		printf '  a raw arm64 Image (no 0x644d5241 at 0x38). QEMU may refuse it.\n' >&2
	fi
elif [ "$machine" != "0xaa64" ]; then
	die "$kernel_src is a PE kernel for machine $machine, not aarch64 (0xaa64). Set ALPINE_ARCH=aarch64 or pass --mirror for an aarch64 tree."
fi
cp "$kernel_src" "$image"
chmod 644 "$image"

mods_root=$work/pkgs/linux-virt/lib/modules
mods_ver=$(CDPATH= cd -- "$mods_root" && ls -1 | head -n1)
[ -n "$mods_ver" ] ||
	die "linux-virt $linux_virt_v has no lib/modules/<version>. The package layout changed."

# ---------------------------------------------------------------- the initramfs

rm -rf "$initramfs_dir"
mkdir -p "$initramfs_dir"/{bin,sbin,lib,etc,proc,sys,dev,root,tmp}

# The whole module tree, at its real nested paths. See the header: flattening it
# would hide the depth/extension combinations the guest's lookup must handle.
#
# mkdir the destination FIRST. With `lib/modules` absent, `cp -R <verdir> lib/modules/`
# does not create `lib/modules/<ver>` — it copies the version directory's CONTENTS
# into `lib/modules` and the version level is lost, so the tree becomes
# lib/modules/kernel/... instead of lib/modules/<ver>/kernel/... . Measured on
# macOS. Every guest glob is anchored on `/lib/modules/*/kernel/...`, so nothing
# matches and the boot dies naming the filesystem. The assertion below turns that
# into a build-time failure with the layout printed.
mkdir -p "$initramfs_dir/lib/modules"
cp -R "$mods_root/$mods_ver" "$initramfs_dir/lib/modules/" 2>/dev/null ||
	die "could not copy the module tree from linux-virt"
if [ ! -d "$initramfs_dir/lib/modules/$mods_ver/kernel" ]; then
	printf 'fetch-artifacts.sh: the module tree landed in the wrong shape. Expected\n' >&2
	printf '  %s/kernel/...\n' "$initramfs_dir/lib/modules/$mods_ver" >&2
	printf '  Found instead:\n' >&2
	ls -1 "$initramfs_dir/lib/modules" >&2 || true
	die "the version level of the module tree is missing; the guest's module lookup would find nothing."
fi
printf 'fetch-artifacts.sh: modules for %s\n' "$mods_ver"

# Verify the modules the guest actually loads are present, and say so HERE rather
# than letting the boot die at mount time naming the filesystem.
missing=""
for m in nbd failover net_failover virtio_net virtio_mmio crc16 crc32c_generic mbcache jbd2 ext4; do
	if ! find "$initramfs_dir/lib/modules" -name "$m.ko*" -print -quit 2>/dev/null | grep -q .; then
		missing="$missing $m"
	fi
done
[ -z "$missing" ] ||
	die "linux-virt $linux_virt_v is missing modules the guest needs:$missing. The kernel's configuration changed; check that CONFIG_BLK_DEV_NBD and CONFIG_EXT4_FS are enabled."

# busybox: the shell and every applet the generated init uses. The applet list is
# explicit because a busybox built without one of them fails inside the guest, in
# a place where the error names something else.
bb_src=$work/pkgs/busybox/bin/busybox
[ -r "$bb_src" ] || die "busybox $busybox_v has no bin/busybox"
cp "$bb_src" "$initramfs_dir/bin/busybox"
chmod 755 "$initramfs_dir/bin/busybox"

for applet in sh mount umount switch_root insmod gunzip ip cat grep ls dd sleep mkdir echo tr cut mknod mdev sed sync poweroff; do
	ln -sf busybox "$initramfs_dir/bin/$applet"
done
# nbd-client must be linked into bin/ as well. Asserting the applet exists is not
# the same as providing it: boot-qemu.sh requires an EXECUTABLE bin/nbd-client, and
# without this symlink the recipe produced artifacts the boot script refused —
# naming a check the artifacts had already satisfied in spirit. (A symlink passes
# `[ -x ]`, which follows the link to the busybox binary.)
ln -sf busybox "$initramfs_dir/bin/nbd-client"
# nbd-client is a busybox applet on Alpine; no separate package exists. Assert it
# is really compiled in rather than assuming, because the failure without it is
# "nbd-client: not found" inside the guest, after a successful boot.
#
# The check must NOT run the binary. This is an aarch64 Linux ELF being inspected
# on whatever host fetched it, and on macOS `busybox --list` fails with "cannot
# execute binary file" — so the applet would be reported MISSING from a busybox
# that has it, and the script would refuse to build working artifacts. Measured,
# on the real package. busybox embeds its applet names in a NUL-terminated string
# table, so an exact-line match over that table answers the question without
# executing anything: `strings | grep -cx` gives 1 for nbd-client and 0 for a name
# that is not an applet. `grep -cx` (not -q) so the comparison is exact: a
# substring match would accept any binary that merely mentions the name.
#
# -n 1 is REQUIRED, not tidiness. `strings` defaults to a 4-character minimum, so
# the short applet names are invisible to it and are reported missing from a
# busybox that has them: measured, `strings | grep -cx sh` gives 0 while
# `strings -n 1 | grep -cx sh` gives 2. Without it this check refuses to build
# working artifacts over `sh`, `ip` and `ash` — the three shortest names the guest
# init depends on.
have_applet() {
	strings -n 1 "$1" 2>/dev/null | grep -cx -- "$2" >/dev/null 2>&1
}
if ! have_applet "$bb_src" nbd-client; then
	die "this busybox ($busybox_v) has no nbd-client applet. The guest cannot connect to NBD. Set ALPINE_RELEASE to a release whose busybox includes it."
fi
# The same check for the applets the generated init needs, so a busybox built
# without one is caught here rather than inside the guest, where the error names
# something else entirely (a failed mount, an unconfigured NIC).
for applet in sh mount insmod gunzip ip switch_root sed sync poweroff ls cat; do
	have_applet "$bb_src" "$applet" ||
		die "this busybox ($busybox_v) has no '$applet' applet, which the guest init requires."
done

# mke2fs and its libraries: needed only by a --mkfs boot. Included unconditionally
# so the FIRST boot works without a second fetch.
if [ -r "$work/pkgs/e2fsprogs/sbin/mke2fs" ]; then
	cp "$work/pkgs/e2fsprogs/sbin/mke2fs" "$initramfs_dir/sbin/mke2fs"
	chmod 755 "$initramfs_dir/sbin/mke2fs"
	ln -sf mke2fs "$initramfs_dir/sbin/mkfs.ext4"
	[ -r "$work/pkgs/e2fsprogs/etc/mke2fs.conf" ] &&
		cp "$work/pkgs/e2fsprogs/etc/mke2fs.conf" "$initramfs_dir/etc/mke2fs.conf"
else
	printf 'fetch-artifacts.sh: WARNING: no sbin/mke2fs in e2fsprogs %s; a --mkfs boot will fail\n' "$e2fsprogs_v" >&2
fi

# The loader must be at the path the binary's INTERP names, and the libraries must
# carry the soname the linker asks for — copying libuuid.so.1.3.0 without the
# libuuid.so.1 symlink leaves mke2fs unable to start. Both are handled explicitly.
find "$work/pkgs/musl" -name 'ld-musl-*' -type f -exec cp {} "$initramfs_dir/lib/" \; 2>/dev/null || true

copy_libs() {
	# copy_libs PKGDIR — every lib*.so* found, plus a soname symlink for each
	# versioned file, so the loader resolves both the exact name and the soname.
	find "$work/pkgs/$1" -name 'lib*.so*' -type f 2>/dev/null | while read -r f; do
		cp "$f" "$initramfs_dir/lib/"
		base=$(basename "$f")
		case "$base" in
		*".so."*)
			soname=$(printf '%s' "$base" | sed 's/\(\.so\.[0-9]*\).*/\1/')
			[ "$soname" != "$base" ] && ln -sf "$base" "$initramfs_dir/lib/$soname"
			;;
		esac
	done
}
for p in e2fsprogs-libs libcom_err libblkid libuuid libeconf; do
	copy_libs "$p"
done

# A missing loader or libext2fs means a --mkfs boot fails inside the guest with
# "not found", so check here where the message can name the package.
if [ -r "$initramfs_dir/sbin/mke2fs" ]; then
	[ -n "$(find "$initramfs_dir/lib" -name 'ld-musl-*' -print -quit 2>/dev/null)" ] ||
		printf 'fetch-artifacts.sh: WARNING: no musl loader in the initramfs; --mkfs will fail\n' >&2
	[ -n "$(find "$initramfs_dir/lib" -name 'libext2fs.so*' -print -quit 2>/dev/null)" ] ||
		printf 'fetch-artifacts.sh: WARNING: no libext2fs in the initramfs; --mkfs will fail\n' >&2
fi

# ---------------------------------------------------------------- pack + verify

# boot-qemu.sh writes the guest /init itself, from the same source that documents
# it, so it is deliberately NOT generated here — one definition, not two that can
# drift. That means the archive below has no /init until boot-qemu.sh packs it.
# The packed copy is provided for convenience only; --initramfs-dir is the path
# that carries a current /init.
cp -R "$initramfs_dir" "$work/pack"
(
	cd "$work/pack"
	find . -print | cpio -o -H newc 2>/dev/null | gzip -9
) >"$dest/initramfs.cpio.gz" 2>/dev/null ||
	printf 'fetch-artifacts.sh: note: cpio is not installed, so no packed initramfs.cpio.gz was written. Pass --initramfs-dir to boot-qemu.sh, which packs it itself.\n' >&2

printf '\nfetch-artifacts.sh: done\n'
printf '  kernel     : %s\n' "$image"
printf '  initramfs  : %s\n' "$initramfs_dir"
printf '  modules    : %s\n' "$mods_ver"
printf '\nBoot it (the first boot needs --mkfs; later boots must not pass it):\n'
printf '  ./test/nbd/boot-qemu.sh --kernel %s \\\n' "$image"
printf '      --initramfs-dir %s --volume-path <share mount> --export <share>/<volume> --mkfs\n' "$initramfs_dir"
printf '\nThe volume must already exist and be owned by the adapter identity (UID/GID 65534);\n'
printf 'boot-qemu.sh checks for it and refuses with the reason if it is missing.\n'
