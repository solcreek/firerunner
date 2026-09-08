#!/bin/bash
# firerunner docker shim — installed as /usr/local/bin/docker on goldens that
# ship Docker, ahead of the real /usr/bin/docker on PATH.
#
# bash, not sh: on Ubuntu /bin/sh is dash, which silently drops environment
# variables whose names are not valid shell identifiers. The runner passes step
# inputs to `docker exec -e INPUT_INCLUDE-HIDDEN-FILES` and expects the docker
# CLI to read the value from its own environment; a dash shim would strip every
# such hyphenated name in transit and the action would see an empty input.
#
# Why: a cache-redirect golden points actions/cache and upload-artifact at the
# host's cache-server by exporting ACTIONS_RESULTS_URL from firerunner-run.sh.
# Steps that run on the guest inherit it from the runner's process environment,
# but steps inside a `container:` job do not: the runner launches those with
# `docker exec -e KEY` for exactly the keys in its own step-environment table,
# which holds only the (renamed, dead) ACTIONS_RESULTS_ORL. Inside the container
# the variable is simply absent, so caching degrades to a miss and artifact
# upload fails.
#
# The job container's environment at *create* time is inherited by every later
# `docker exec`, so the one place to inject the URL is the runner's own
# `docker create`. This shim does exactly that and nothing else: it only acts
# when ACTIONS_RESULTS_URL is set (cache-redirect + a configured cache), only
# for `create`, and only when the caller is Runner.Worker — a job's own docker
# usage passes through untouched.
set -eu

# The real binary is looked up in fixed locations that exclude this shim's own
# directory, so the shim can never re-exec itself. FIRERUNNER_SHIM_REAL_DOCKER
# lets the test suite substitute a fake; like FIRERUNNER_SHIM_PPID below it
# crosses no privilege boundary (a job already controls its own processes, and
# cannot reach the runner's environment).
real="${FIRERUNNER_SHIM_REAL_DOCKER:-}"
if [ -z "$real" ]; then
	for d in /usr/bin /bin /usr/sbin; do
		if [ -x "$d/docker" ]; then
			real="$d/docker"
			break
		fi
	done
fi
if [ -z "$real" ] || [ ! -x "$real" ]; then
	echo "firerunner docker shim: real docker binary not found" >&2
	exit 127
fi

# FIRERUNNER_SHIM_PPID exists so the test suite can stand in for the runner;
# a job that sets it only ever decorates its own containers.
inject=0
if [ -n "${ACTIONS_RESULTS_URL:-}" ] && [ "${1:-}" = "create" ]; then
	parent="$(cat "/proc/${FIRERUNNER_SHIM_PPID:-$PPID}/comm" 2>/dev/null || true)"
	[ "$parent" = "Runner.Worker" ] && inject=1
fi

if [ "$inject" = 1 ]; then
	shift
	exec "$real" create \
		-e "ACTIONS_RESULTS_URL=$ACTIONS_RESULTS_URL" \
		-e "ACTIONS_CACHE_SERVICE_V2=${ACTIONS_CACHE_SERVICE_V2:-true}" \
		"$@"
fi
exec "$real" "$@"
