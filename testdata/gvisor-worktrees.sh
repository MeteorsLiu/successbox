#!/bin/sh
set -eu
if test "$#" != 5; then
    printf 'usage: %s SBX BACKEND RUNSC WORKTREES NEW_TEST_DIRECTORY\n' "$0" >&2
    exit 2
fi
client=$1
backend=$2
runtime=$3
helper=$4
stage=$5
test ! -e "$stage"

mkdir -p "$stage/sources"
chmod 711 "$stage"
"$backend" --socket "$stage/control.sock" --state "$stage/state" --runsc "$runtime" --worktrees "$helper" >"$stage/service.log" 2>&1 &
service=$!
box=
cleanup() {
 result=$?
 trap - EXIT
 if test -n "$box"; then "$client" --socket "$stage/control.sock" close "$box" || result=1; fi
 kill -TERM "$service" || true
 wait "$service" || result=1
 if test "$result" != 0; then cat "$stage/service.log"; for log in "$stage/state"/*/runtime.log; do test ! -f "$log" || cat "$log"; done; fi
 exit "$result"
}
trap cleanup EXIT
while ! test -S "$stage/control.sock"; do kill -0 "$service" || exit 1; sleep .1; done
for uid in 0 1000; do
 repo=$stage/sources/repo-$uid
 mkdir -p "$repo"
 git -C "$repo" init -q
 printf original > "$repo/file"
 git -C "$repo" add .
 git -C "$repo" -c user.name=Test -c user.email=test@example.invalid commit -qm initial
 chown -R "$uid:$uid" "$repo"
 printf '{"rootfs":"/","workingDir":"%s","user":{"uid":%s,"gid":%s}}\n' "$repo" "$uid" "$uid" > "$stage/config.json"
 box=$("$client" --socket "$stage/control.sock" create "$stage/config.json")
 "$client" --socket "$stage/control.sock" exec --sandbox "$box" -- /bin/sh -c 'set -eu; test "$(cat file)" = original; printf guest > file; git diff --stat; test ! -e "$1/state/results"' probe "$stage"
 test "$(cat "$repo/file")" = original
 "$client" --socket "$stage/control.sock" close "$box"
 box=
 git -c safe.directory="$repo" -C "$repo" worktree list --porcelain
 count=$(git -c safe.directory="$repo" -C "$repo" worktree list --porcelain | grep -c '^worktree ')
 test "$count" = 2
 done
printf 'RPC worktrees: root and UID 1000, bootstrap cwd, hidden state, Close registration: PASS\n'
