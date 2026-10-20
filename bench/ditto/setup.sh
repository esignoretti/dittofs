#!/usr/bin/env bash
# Sets up a Linux host for night.sh, as root, so a replaced host is ready again in minutes. It can
# run again at any time; it changes only what is missing or different.
#   setup.sh                      packages, the run's user, directories and the checkout
#   setup.sh cron                 the cron file that starts the night; after a smoke run passed
#   setup.sh remove               undoes all of the above but the packages
#   setup.sh split-drive DEV --yes
#                                 not part of the setup: takes DEV out of the host's RAID1 mirrors and
#                                 mounts it alone at /srv/perf/data, so the runs' writes stop
#                                 landing on the OS drive. The OS loses its redundancy, so the
#                                 host's administrator decides when.
# Environment: BRANCH (the checkout's branch, default develop), PERF_USER (dittofs-perf), SLACK_DIR
# and SCENARIO_HOST (written into the cron file for the nightly post).
set -euo pipefail

CHECKOUT=/srv/perf/dittofs
CRON=/etc/cron.d/dittofs-perf
PERF_USER="${PERF_USER:-dittofs-perf}"

# installed PACKAGE: whether PACKAGE is installed, not merely known (a removed one keeps its config).
installed() { dpkg-query -W -f='${db:Status-Abbrev}' "$1" 2>/dev/null | grep -q '^ii'; }

install() {
    local p missing=()
    for p in fio samba cifs-utils nvme-cli python3 git iproute2 util-linux; do installed "$p" || missing+=("$p"); done
    if test "${#missing[@]}" -gt 0; then
        apt-get update -qq
        apt-get install -y -qq --no-install-recommends "${missing[@]}" >/dev/null
    fi
    # The run starts its own smbd; a host that had no Samba keeps none running.
    if [[ " ${missing[*]} " == *" samba "* ]] && command -v systemctl >/dev/null; then
        systemctl disable --now smbd nmbd 2>/dev/null || true
    fi
    # The Unix account the Samba baseline maps its SMB user to; the DittoFS user gets its uid.
    id "$PERF_USER" >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin "$PERF_USER"
    mkdir -p /srv/perf/data /srv/perf/mnt /var/tmp/dittofs-perf
    test -d "$CHECKOUT/.git" ||
        git clone -q --branch "${BRANCH:-develop}" https://github.com/marmos91/dittofs "$CHECKOUT"
}

cron() {
    cat >"$CRON" <<EOF
# DittoFS nightly performance run (bench/ditto in $CHECKOUT): as root from 18:45 to its hard stop
# at 05:45, under the lock of the scenario runs (/var/tmp/dittofs-scenarios), so they never overlap.
# Results and logs: /var/tmp/dittofs-perf. Remove with: $CHECKOUT/bench/ditto/setup.sh remove
# CALIBRATION=yes runs every point three times and posts nothing: the first week. With no, it
# posts one summary a night to the channel in SLACK_DIR.
CALIBRATION=yes
SLACK_DIR=${SLACK_DIR:-/home/lucian-lungu/.config/dittofs-scenarios}
SCENARIO_HOST=${SCENARIO_HOST:-$(hostname -s)}
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
45 18 * * * root $CHECKOUT/bench/ditto/night.sh >>/var/tmp/dittofs-perf/cron.log 2>&1
EOF
    chmod 644 "$CRON"
}

remove() {
    rm -f "$CRON"
    umount /srv/perf/mnt/* 2>/dev/null || true
    rm -rf /srv/perf /var/tmp/dittofs-perf
    ! id "$PERF_USER" >/dev/null 2>&1 || userdel "$PERF_USER"
}

# split_drive DEV: fails and removes DEV's partitions from every md array, then makes DEV one ext4
# file system mounted at /srv/perf/data. To undo: unmount it and drop its fstab line, copy the other
# drive's partition table onto DEV (sfdisk -d OTHER | sfdisk DEV), add each partition back to its
# array (mdadm /dev/mdN --add PART), wait for the resync, then grub-install DEV.
split_drive() {
    local dev="$1"
    test "${2:-}" = --yes || { echo "setup.sh split-drive $dev --yes: it breaks the RAID1 mirror" >&2; exit 2; }
    awk '/^md/ { print $1 }' /proc/mdstat | while read -r md; do
        mdadm --detail "/dev/$md" | awk -v dev="$dev" 'index($NF, dev) == 1 { print $NF }' | while read -r part; do
            mdadm "/dev/$md" --fail "$part" --remove "$part"
            mdadm --zero-superblock "$part"
        done
    done
    wipefs -a "$dev"
    parted -s "$dev" mklabel gpt mkpart dittofs-perf ext4 1MiB 100%
    udevadm settle
    mkfs.ext4 -q -L dittofs-perf "${dev}p1"
    grep -q '^LABEL=dittofs-perf ' /etc/fstab || echo 'LABEL=dittofs-perf /srv/perf/data ext4 defaults,noatime 0 2' >>/etc/fstab
    mkdir -p /srv/perf/data
    mount /srv/perf/data
}

test "$(id -u)" -eq 0 || { echo "setup.sh: run it as root" >&2; exit 2; }
case "${1:-}" in
"") install ;;
cron) cron ;;
remove) remove ;;
split-drive) split_drive "${2:?setup.sh split-drive DEV --yes}" "${3:-}" ;;
*) echo "usage: setup.sh [cron | remove | split-drive DEV --yes]" >&2; exit 2 ;;
esac
