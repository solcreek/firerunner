#!/usr/bin/env bash
# Unit tests for assets/docker-shim.sh. Runs anywhere with bash + coreutils: a
# fake docker records its argv, and a copy of `sleep` named Runner.Worker stands
# in for the runner so /proc/<pid>/comm reads "Runner.Worker".
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SHIM="$HERE/assets/docker-shim.sh"
TMP="$(mktemp -d)"
trap 'kill "${WORKER_PID:-}" 2>/dev/null || true; rm -rf "$TMP"' EXIT

# Fake docker: one argv element per line, then exit with $FAKE_DOCKER_EXIT.
cat > "$TMP/docker" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' "$@" > "$FAKE_DOCKER_ARGV"
exit "${FAKE_DOCKER_EXIT:-0}"
FAKE
chmod +x "$TMP/docker"
export FIRERUNNER_SHIM_REAL_DOCKER="$TMP/docker"
export FAKE_DOCKER_ARGV="$TMP/argv"

# A process whose comm is "Runner.Worker".
cp "$(command -v sleep)" "$TMP/Runner.Worker"
"$TMP/Runner.Worker" 300 &
WORKER_PID=$!
for _ in $(seq 1 50); do
	[ "$(cat /proc/$WORKER_PID/comm 2>/dev/null)" = "Runner.Worker" ] && break
	sleep 0.05
done
[ "$(cat /proc/$WORKER_PID/comm)" = "Runner.Worker" ] || { echo "could not stage a Runner.Worker parent"; exit 1; }

fails=0
check() { # name expected-argv-file
	if diff -u "$2" "$FAKE_DOCKER_ARGV" >"$TMP/diff"; then
		echo "ok   $1"
	else
		echo "FAIL $1"; cat "$TMP/diff"; fails=$((fails+1))
	fi
}
expect() { printf '%s\n' "$@" > "$TMP/want"; }
run_shim() { : > "$FAKE_DOCKER_ARGV"; env "$@"; }

URL="http://172.19.3.1:8099/"

# 1. Runner.Worker + create + URL set -> injected right after `create`, original
#    options preserved (including an argument containing spaces).
run_shim ACTIONS_RESULTS_URL="$URL" FIRERUNNER_SHIM_PPID="$WORKER_PID" \
	"$SHIM" create --name abc -e "FOO=bar baz" --entrypoint tail img:1 -f /dev/null
expect create -e "ACTIONS_RESULTS_URL=$URL" -e "ACTIONS_CACHE_SERVICE_V2=true" --name abc -e "FOO=bar baz" --entrypoint tail img:1 -f /dev/null
check "injects into runner's docker create" "$TMP/want"

# 2. An existing ACTIONS_CACHE_SERVICE_V2 value is forwarded, not overwritten.
run_shim ACTIONS_RESULTS_URL="$URL" ACTIONS_CACHE_SERVICE_V2=false FIRERUNNER_SHIM_PPID="$WORKER_PID" \
	"$SHIM" create img
expect create -e "ACTIONS_RESULTS_URL=$URL" -e "ACTIONS_CACHE_SERVICE_V2=false" img
check "forwards existing ACTIONS_CACHE_SERVICE_V2" "$TMP/want"

# 3. No URL (non-redirect golden, or cache not configured) -> pure passthrough.
run_shim -u ACTIONS_RESULTS_URL FIRERUNNER_SHIM_PPID="$WORKER_PID" \
	"$SHIM" create --name abc img
expect create --name abc img
check "passthrough when ACTIONS_RESULTS_URL unset" "$TMP/want"

# 4. Other subcommands are never touched, even from the runner with a URL.
for sub in exec run ps pull start; do
	run_shim ACTIONS_RESULTS_URL="$URL" FIRERUNNER_SHIM_PPID="$WORKER_PID" \
		"$SHIM" "$sub" -e ACTIONS_RESULTS_ORL abc sh -c 'echo hi'
	expect "$sub" -e ACTIONS_RESULTS_ORL abc sh -c 'echo hi'
	check "passthrough for docker $sub" "$TMP/want"
done

# 5. A job's own `docker create` (parent is not Runner.Worker) is untouched.
run_shim ACTIONS_RESULTS_URL="$URL" FIRERUNNER_SHIM_PPID="$$" \
	"$SHIM" create --name mine img
expect create --name mine img
check "passthrough when caller is not Runner.Worker" "$TMP/want"

# 6. A bogus parent pid (comm unreadable) is treated as not-the-runner.
run_shim ACTIONS_RESULTS_URL="$URL" FIRERUNNER_SHIM_PPID="999999999" \
	"$SHIM" create img
expect create img
check "passthrough when parent comm unreadable" "$TMP/want"

# 7. The real docker's exit code propagates.
set +e
run_shim ACTIONS_RESULTS_URL="$URL" FIRERUNNER_SHIM_PPID="$WORKER_PID" FAKE_DOCKER_EXIT=3 "$SHIM" create img
rc=$?
set -e
if [ "$rc" = 3 ]; then echo "ok   propagates exit code"; else echo "FAIL exit code = $rc, want 3"; fails=$((fails+1)); fi

# 8. Missing real docker -> 127 with a message, no crash.
set +e
out="$(FIRERUNNER_SHIM_REAL_DOCKER="$TMP/does-not-exist" "$SHIM" ps 2>&1)"; rc=$?
set -e
if [ "$rc" = 127 ] && [[ "$out" == *"real docker binary not found"* ]]; then
	echo "ok   missing real docker -> 127"
else
	echo "FAIL missing real docker: rc=$rc out=$out"; fails=$((fails+1))
fi

# 9. Unset override falls back to the fixed search list and never picks itself:
#    with an empty search result it must fail cleanly rather than recurse.
mkdir -p "$TMP/lonely"; cp "$SHIM" "$TMP/lonely/docker"
set +e
out="$(env -u FIRERUNNER_SHIM_REAL_DOCKER PATH="$TMP/lonely" ACTIONS_RESULTS_URL="$URL" \
	bash -c 'exec docker create img' 2>&1)"; rc=$?
set -e
case "$rc" in
	127) echo "ok   no recursion without a real docker (rc=127)";;
	0)   echo "ok   no recursion (real docker present on this host)";;
	*)   echo "FAIL recursion guard: rc=$rc out=$out"; fails=$((fails+1));;
esac

[ "$fails" = 0 ] && echo "all docker-shim tests passed" || { echo "$fails test(s) failed"; exit 1; }
