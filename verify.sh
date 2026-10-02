#!/bin/sh
# Verifies the assembled jev-cli image from the inside.
#
# Invoked as:
#   docker run --rm --env-file buildargs.conf -e TARGET=jev \
#     -v "$(pwd)/verify.sh:/verify.sh:ro" --entrypoint /bin/sh <ref> /verify.sh
#
# The image is FROM scratch (no shell), so the shared verify-image action
# derives a throwaway image that layers busybox's /bin onto the exact digest
# under test; this script then runs under that busybox ash as the image's
# default user. Run by hand against a local build, derive the same overlay:
#   printf 'FROM jev-cli:local\nCOPY --from=busybox:musl /bin /bin\n' \
#     | docker build -q -t jev-cli-verify -
#
# No network and no real API key are used. The last check runs the real
# binary through its full startup path (config, audit log and credential
# resolution) with the key unset and expects the documented hard-error exit.
#
# Runs under busybox ash, dash and bash: POSIX sh only, no arrays/[[/local.

fails=0
ok()  { echo "ok   $*"; }
bad() { echo "FAIL $*"; fails=$((fails + 1)); }

check_uid() {
    if [ "$(id -u)" = "$1" ]; then ok "runs as uid $1"; else bad "runs as uid $(id -u), want $1"; fi
}

check_env() {
    if [ "$(printenv "$1")" = "$2" ]; then ok "env $1=$2"; else bad "env $1=$(printenv "$1"), want $2"; fi
}

# Exit code of a command, with stdout and stderr both captured into $out.
check_exit() {
    want=$1; shift
    out=$("$@" 2>&1 </dev/null)
    rc=$?
    if [ "$rc" = "$want" ]; then ok "$* exits $want"; else bad "$* exits $rc, want $want: $out"; fi
}

echo "verifying jev-cli image: ${TARGET:-unset}"

check_uid 1001
check_env HOME /tmp

# HOME must be writable by the default user: the audit log and config dir
# resolve beneath it.
if touch "$HOME/.verify-write" 2>/dev/null; then
    rm -f "$HOME/.verify-write"
    ok "HOME $HOME is writable"
else
    bad "HOME $HOME is not writable"
fi

if [ -x /usr/local/bin/jev ]; then ok "executable: /usr/local/bin/jev"; else bad "not executable: /usr/local/bin/jev"; fi
if [ -s /etc/ssl/certs/ca-certificates.crt ]; then ok "CA bundle present"; else bad "CA bundle missing: /etc/ssl/certs/ca-certificates.crt"; fi

check_exit 0 /usr/local/bin/jev --help
check_exit 0 /usr/local/bin/jev check --help

# Full startup path with no key: must reach the credential lookup and fail
# there (exit 3, message names OpenRouter), not earlier on HOME/config/audit
# resolution. env -u keeps the check valid even if the caller's environment
# carries a key.
out=$(echo '{"context":"x","propositions":["y"]}' \
    | env -u OPENROUTER_API_KEY /usr/local/bin/jev check -j - -o json 2>&1 >/dev/null)
rc=$?
case "$rc:$out" in
    3:*[Oo]pen[Rr]outer*) ok "jev check without a key exits 3 naming OpenRouter" ;;
    *)                    bad "jev check without a key: exit $rc, want 3 naming OpenRouter: $out" ;;
esac

exit $((fails > 0))
