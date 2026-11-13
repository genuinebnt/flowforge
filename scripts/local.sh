#!/usr/bin/env bash
# local.sh: the local multi-process topology of deploy/compose.local.yml
# (G-OPS-21). Every service runs in its own container against one Postgres
# with the Postgres event bus; no cloud account is used.
#
#   scripts/local.sh up      # write deploy/.local/local.env once, build, start
#   scripts/local.sh down    # stop and delete the database volume
#   scripts/local.sh config  # validate the compose file
#
# deploy/.local/local.env holds the shared internal signing key
# (FF_SIGNING_KID, FF_SIGNING_KEY_<KID>): the env-file secret every service
# and the gateway read. It is generated with a random seed; deploy/.gitignore
# keeps it out of git and deploy/Dockerfile.dockerignore out of images.
set -euo pipefail

cd "$(dirname "$0")/../deploy"
COMPOSE=(docker compose -f compose.local.yml)

ensure_env() {
	mkdir -p .local/blobs
	chmod 0777 .local/blobs # the images run as uid 65532
	if [ ! -f .local/local.env ]; then
		umask 077
		seed="$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')"
		printf 'FF_SIGNING_KID=local-1\nFF_SIGNING_KEY_LOCAL_1=%s\n' "$seed" >.local/local.env
		echo "wrote deploy/.local/local.env (new signing key)"
	fi
}

case "${1:-}" in
up)
	ensure_env
	"${COMPOSE[@]}" up -d --build
	echo "gateway: http://127.0.0.1:${FF_GATEWAY_PORT:-8080}/v1"
	;;
down)
	"${COMPOSE[@]}" down -v
	;;
config)
	ensure_env
	"${COMPOSE[@]}" config --quiet && echo "compose.local.yml: ok"
	;;
*)
	echo "usage: $0 up|down|config" >&2
	exit 2
	;;
esac
