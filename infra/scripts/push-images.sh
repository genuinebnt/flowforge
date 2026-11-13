#!/usr/bin/env bash
# Build every Flowforge image with deploy/Dockerfile, push it to the
# environment's Artifact Registry repository and write the digests to
# infra/envs/<env>/images.auto.tfvars.json, which terraform picks up on the
# next plan. Needed once, for the first apply (afterwards the deploy
# workflow rolls images forward by digest).
#
#   infra/scripts/push-images.sh <lab|dev|prod>
#
# The repository must exist first:
#   terraform -chdir=infra/envs/<env> apply -target=google_artifact_registry_repository.images
# Needs docker and gcloud (gcloud auth configure-docker <region>-docker.pkg.dev).
set -euo pipefail

env="${1:?usage: push-images.sh <lab|dev|prod>}"
root="$(cd "$(dirname "$0")/../.." && pwd)"
dir="$root/infra/envs/$env"
tfvar() { sed -n "s/^$1 *= *\"\\([^\"]*\\)\".*/\\1/p" "$dir/terraform.tfvars" | head -n1; }

project="${GCP_PROJECT:-$(tfvar project_id)}"
region="${GCP_REGION:-$(tfvar region)}"
registry="$region-docker.pkg.dev/$project/flowforge"
tag="bootstrap-$(git -C "$root" rev-parse --short=12 HEAD)-$(date +%s)"
services="gateway engine scope graph notifier similarity tool-runner content egress-proxy migrate"

out="$dir/images.auto.tfvars.json"
{
	echo '{'
	echo '  "images": {'
	sep=""
	for s in $services; do
		docker build -f "$root/deploy/Dockerfile" --build-arg SERVICE="$s" --build-arg VERSION="$tag" \
			-t "$registry/$s:$tag" "$root" >&2
		docker push "$registry/$s:$tag" >&2
		digest="$(docker inspect --format='{{index .RepoDigests 0}}' "$registry/$s:$tag")"
		printf '%s    "%s": "%s"' "$sep" "$s" "$registry/$s@${digest##*@}"
		sep=$',\n'
	done
	printf '\n  }\n}\n'
} >"$out"
echo "wrote $out" >&2
