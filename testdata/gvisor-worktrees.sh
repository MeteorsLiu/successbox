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
 plain=$stage/plain-$uid
 readonly=$stage/readonly-$uid
 mkdir -p "$plain" "$readonly"
 printf original > "$plain/file"
 printf readonly > "$readonly/file"
 chown -R "$uid:$uid" "$repo" "$plain"
 printf '{"rootfs":"/","workingDir":"%s","user":{"uid":%s,"gid":%s},"mounts":[{"source":"%s","destination":"/workspace"},{"source":"%s","destination":"/single-file"},{"source":"%s","destination":"/workspace/readonly","readOnly":true},{"source":"%s","destination":"/state-alias"}]}\n' "$repo" "$uid" "$uid" "$plain" "$plain/file" "$readonly" "$stage/state" > "$stage/config.json"
 box=$("$client" --socket "$stage/control.sock" create "$stage/config.json")
 "$client" --socket "$stage/control.sock" exec --sandbox "$box" -- /bin/sh -c 'set -eu; test "$(cat file)" = original; printf guest > file; git diff --stat; test ! -e "$1/state/results"; test ! -e /state-alias/results; printf changed > /workspace/file; printf separate > /single-file; test "$(cat /workspace/file)" = changed; test "$(cat /workspace/readonly/file)" = readonly; if (printf wrong > /workspace/readonly/file) 2>/dev/null; then exit 1; fi' probe "$stage"
 test "$(cat "$repo/file")" = original
 test "$(cat "$plain/file")" = original
 test "$(cat "$readonly/file")" = readonly
 "$client" --socket "$stage/control.sock" close "$box"
 box=
 git -c safe.directory="$repo" -C "$repo" worktree list --porcelain
 count=$(git -c safe.directory="$repo" -C "$repo" worktree list --porcelain | grep -c '^worktree ')
 test "$count" = 2
 # Multiple host worktrees share one private Git store, with independent indexes.
 for number in 1 2 3 4 5 6 7 8; do
  git -c safe.directory="$repo" -C "$repo" worktree add --detach --quiet "$stage/sources/linked-$uid-$number"
  chown -R "$uid:$uid" "$stage/sources/linked-$uid-$number"
 done
 chown -R "$uid:$uid" "$repo/.git"
 box=$("$client" --socket "$stage/control.sock" create "$stage/config.json")
 pids=
 for number in 1 2 3 4 5 6 7 8; do
  "$client" --socket "$stage/control.sock" exec --sandbox "$box" -- /bin/sh -ec 'cd "$1"; test "$(cat file)" = original; printf "%s" "$2" > file; git add file; git -c user.name=Test -c user.email=test@example.invalid commit -qm private' probe "$stage/sources/linked-$uid-$number" "guest-$number" &
  pids="$pids $!"
 done
 for pid in $pids; do wait "$pid"; done
 set -- "$stage/state/results/$box"/common-*
 test "$#" = 1
 for number in 1 2 3 4 5 6 7 8; do
  test "$(cat "$stage/sources/linked-$uid-$number/file")" = original
 done
 results=$stage/state/results/$box
 "$client" --socket "$stage/control.sock" close "$box"
 box=
 count=0
 for result in "$results"/repo-*/worktree; do
  git -c safe.directory="$repo" -C "$repo" worktree list --porcelain | grep -Fx "worktree $result"
  value=$(cat "$result/file")
  test "$value" = original || case "$value" in guest-[1-8]) count=$((count + 1));; *) exit 1;; esac
 done
 test "$count" = 8
 done
printf 'RPC worktrees: root and UID 1000, bootstrap cwd, private directory/file binds, read-only mounts, hidden state, Close registration: PASS\n'
