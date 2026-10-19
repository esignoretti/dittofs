#!/usr/bin/env bash
# A local FoundationDB for the fdbkv backend's tests: the client library, a
# cluster in containers, and go test pointed at both.
#
#   test/fdb/cluster.sh client      download libfdb_c and its headers
#   test/fdb/cluster.sh up [1|3]    start N fdbserver processes (default 1); three
#                                   run double replication with three coordinators
#   test/fdb/cluster.sh down        stop and remove them, and their data
#   test/fdb/cluster.sh kill N      kill process N (1-based) without a clean stop
#   test/fdb/cluster.sh start N     start a killed process N again
#   test/fdb/cluster.sh test ...    go test from the repository root, with the
#                                   client library on the cgo paths and
#                                   DITTOFS_FDB_CLUSTER_FILE set
#
# Host networking, so the cluster file's 127.0.0.1 addresses are the ones the
# test client dials. That works on Linux only; on macOS run it inside a Linux
# VM. The client library is x86_64 only here. Docker or podman.
set -euo pipefail

VERSION="${FDB_VERSION:-7.3.77}"
RUNTIME="${CONTAINER_RUNTIME:-$(command -v docker || command -v podman)}"
# On disk, not under /tmp: /tmp is a RAM disk on many Linux hosts, which hides
# the cost of every commit's sync.
BASE="${DITTOFS_FDB_DIR:-$HOME/.cache/dittofs-fdb}"
DATA="$BASE/data"
CLIENT="$BASE/client-$VERSION"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
IMAGE="docker.io/foundationdb/foundationdb:$VERSION"

client() {
    local base="https://github.com/apple/foundationdb/releases/download/$VERSION"
    local tmp
    tmp="$(mktemp -d)"
    trap 'rm -rf "$tmp"' RETURN
    curl -fsSL -o "$tmp/libfdb_c.so" "$base/libfdb_c.x86_64.so"
    curl -fsSL -o "$tmp/headers.tar.gz" "$base/fdb-headers-$VERSION.tar.gz"
    # fdb_c.h includes fdb_c_types.h, which the release's header tarball leaves out.
    curl -fsSL -o "$tmp/fdb_c_types.h" \
        "https://raw.githubusercontent.com/apple/foundationdb/$VERSION/bindings/c/foundationdb/fdb_c_types.h"
    # The release's own digests (and the tagged header's), so a tampered
    # download never reaches LD_LIBRARY_PATH. Another FDB_VERSION needs its own.
    (cd "$tmp" && sha256sum -c - <<EOF) || exit 1
617572467897d0360f76ffc33f24b58090ee691e3273750d2ff334cc71aae3fa  libfdb_c.so
73b1efca62bf1779e0d4d62131514c96f689db82865695618086e73a057ea385  headers.tar.gz
82ea67e61592873c637e228764f5921179a710826be558ee0d1703023913e14d  fdb_c_types.h
EOF
    mkdir -p "$CLIENT/lib" "$CLIENT/include/foundationdb"
    mv "$tmp/libfdb_c.so" "$CLIENT/lib/"
    tar -xzf "$tmp/headers.tar.gz" -C "$CLIENT/include/foundationdb"
    mv "$tmp/fdb_c_types.h" "$CLIENT/include/foundationdb/"
    echo "client $VERSION in $CLIENT"
}

process_up() {
    local n=$1 port=$((4499 + $1))
    mkdir -p "$DATA/p$n"
    "$RUNTIME" run -d --name "dittofs-fdb$n" --network host \
        -v "$DATA:/cluster" --entrypoint /usr/bin/fdbserver "$IMAGE" \
        --cluster-file /cluster/fdb.cluster --public-address "127.0.0.1:$port" \
        --listen-address "127.0.0.1:$port" --datadir "/cluster/p$n" --logdir "/cluster/p$n" \
        --locality-zoneid "z$n" --locality-machineid "m$n" >/dev/null
}

fdbcli() {
    "$RUNTIME" exec dittofs-fdb1 /usr/bin/fdbcli -C /cluster/fdb.cluster --exec "$1"
}

up() {
    local n=$1 coords="" mode=single
    for i in $(seq 1 "$n"); do coords+="${coords:+,}127.0.0.1:$((4499 + i))"; done
    # double, not triple: triple places logs in three zones, so three processes
    # cannot lose one. double keeps two durable copies and survives one loss.
    [[ "$n" -ge 3 ]] && mode=double
    echo "dittofs:dittofs@$coords" >"$DATA/fdb.cluster"
    for i in $(seq 1 "$n"); do process_up "$i"; done
    sleep 2
    fdbcli "configure new $mode ssd" >/dev/null
    for _ in $(seq 1 60); do
        if fdbcli "status minimal" 2>/dev/null | grep -q "available"; then
            echo "FoundationDB $VERSION up: $n process(es), $mode replication; cluster file $DATA/fdb.cluster"
            return 0
        fi
        sleep 1
    done
    fdbcli "status" >&2 || true
    return 1
}

# Created here, as the user: a bind mount of a missing directory would have the
# container runtime create it as root, and nothing after could write to it.
mkdir -p "$DATA"

case "${1:-}" in
client) client ;;
up) up "${2:-1}" ;;
down)
    for i in 1 2 3; do "$RUNTIME" rm -f "dittofs-fdb$i" >/dev/null 2>&1 || true; done
    # The processes may write as root; remove the data from inside a container.
    "$RUNTIME" run --rm -v "$DATA:/d" docker.io/library/busybox:1.37 sh -c 'rm -rf /d/*' >/dev/null 2>&1 || true
    ;;
kill) "$RUNTIME" kill -s KILL "dittofs-fdb${2:?process number}" >/dev/null ;;
start) "$RUNTIME" start "dittofs-fdb${2:?process number}" >/dev/null ;;
test)
    shift
    [[ -f "$CLIENT/lib/libfdb_c.so" ]] || client
    export CGO_CFLAGS="-I$CLIENT/include" CGO_LDFLAGS="-L$CLIENT/lib"
    export LD_LIBRARY_PATH="$CLIENT/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
    export DITTOFS_FDB_CLUSTER_FILE="${DITTOFS_FDB_CLUSTER_FILE:-$DATA/fdb.cluster}"
    cd "$ROOT" && exec go test "$@"
    ;;
*)
    sed -n '2,17p' "$0"
    exit 2
    ;;
esac
