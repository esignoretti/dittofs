#!/usr/bin/env bash
# The ordinary user cthon04 and nfstest run their tests as. Sourced, not
# executed; the functions need root.
#
# As root the tests would mostly exercise root's permission bypass: nfstest
# grants every read and write check to a file root owns, and cthon04's
# op_chmod passes whatever the server does. The export squashes root
# (root_to_guest), and this user gets a read-write grant on it from the setup
# step: TEST_USER_NAME and TEST_USER_UID must match the --test-user that
# test/conformance/suites.json passes to setup-posix.sh.
#
# nfstest expects the user to have passwordless sudo, for mount and tcpdump,
# and passwordless ssh to its second client.

TEST_USER_NAME=dfsnfstest
TEST_USER_UID=64000
TEST_USER_SUDOERS="/etc/sudoers.d/$TEST_USER_NAME"

# test_user_home — the user's home directory.
test_user_home() { getent passwd "$TEST_USER_NAME" | cut -d: -f6; }

# test_user_ensure — create the user if it is missing, with sudo and an ssh key
# that lets it log in to itself.
test_user_ensure() {
    if ! getent group "$TEST_USER_NAME" >/dev/null; then
        groupadd -g "$TEST_USER_UID" "$TEST_USER_NAME" || return 1
    fi
    if ! id -u "$TEST_USER_NAME" >/dev/null 2>&1; then
        useradd -m -u "$TEST_USER_UID" -g "$TEST_USER_UID" -s /bin/bash "$TEST_USER_NAME" || return 1
    fi
    [[ "$(id -u "$TEST_USER_NAME")" == "$TEST_USER_UID" ]] || {
        echo "$TEST_USER_NAME exists with uid $(id -u "$TEST_USER_NAME"), not $TEST_USER_UID" >&2
        return 1
    }
    # sshd refuses a locked account ('!' in the password field) even for a key.
    usermod -p '*' "$TEST_USER_NAME" || return 1

    echo "$TEST_USER_NAME ALL=(ALL) NOPASSWD: ALL" >"$TEST_USER_SUDOERS"
    chmod 0440 "$TEST_USER_SUDOERS"
    visudo -cqf "$TEST_USER_SUDOERS" || { rm -f "$TEST_USER_SUDOERS"; return 1; }

    local home ssh
    home="$(test_user_home)"
    ssh="$home/.ssh"
    install -d -m 0700 -o "$TEST_USER_NAME" -g "$TEST_USER_NAME" "$ssh"
    if [[ ! -f "$ssh/id_ed25519" ]]; then
        ssh-keygen -q -t ed25519 -N '' -f "$ssh/id_ed25519" || return 1
    fi
    cp "$ssh/id_ed25519.pub" "$ssh/authorized_keys"
    # The second client is a fresh namespace every run, with a fresh host key.
    printf '%s\n' 'Host *' '    StrictHostKeyChecking no' \
        '    UserKnownHostsFile /dev/null' '    LogLevel ERROR' >"$ssh/config"
    chown -R "$TEST_USER_NAME:$TEST_USER_NAME" "$ssh"
    chmod 0600 "$ssh/id_ed25519" "$ssh/authorized_keys" "$ssh/config"
}

# test_user_release — withdraw the sudo grant; the account itself is harmless.
test_user_release() { rm -f "$TEST_USER_SUDOERS"; }

# test_user_sshd_config DIR ADDR — write an sshd config that admits only the
# test user, on ADDR, with a host key generated into DIR. Prints its path.
test_user_sshd_config() {
    local dir="$1" addr="$2"
    mkdir -p "$dir"
    [[ -f "$dir/host_key" ]] || ssh-keygen -q -t ed25519 -N '' -f "$dir/host_key" || return 1
    cat >"$dir/sshd_config" <<EOF
ListenAddress $addr
HostKey $dir/host_key
PidFile /run/sshd-dfsnfs.pid
AllowUsers $TEST_USER_NAME
PubkeyAuthentication yes
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
PrintMotd no
EOF
    echo "$dir/sshd_config"
}

# test_user_copy SRC DEST — a copy of SRC at DEST that the user owns. The
# suites are fetched into the checkout, which an ordinary user may not be able
# to reach: a CI runner's home directory is not world-readable.
test_user_copy() {
    rm -rf "$2"
    mkdir -p "$(dirname "$2")"
    cp -a "$1" "$2" && chown -R "$TEST_USER_NAME:$TEST_USER_NAME" "$2"
}
