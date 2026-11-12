#!/usr/bin/env bash
# Roll one Cloud Run service forward to an image digest (spec 17.6):
# deploy a new revision with no traffic, send it 10 %, soak, check the
# revision is ready, then 100 %. Terraform owns every other setting of the
# service; `gcloud run deploy --image` only changes the image.
#
#   rollout.sh <service> <image@sha256:digest>
#
# Env: GCP_REGION (required), CANARY_SOAK_SECONDS (default 300).
set -euo pipefail

svc="$1"
image="$2"
region="${GCP_REGION:?GCP_REGION is required}"
soak="${CANARY_SOAK_SECONDS:-300}"
name="ff-${svc}"

case "$image" in
*@sha256:*) ;;
*) echo "rollout: $image is not pinned by digest" >&2; exit 1 ;;
esac

tag="r-${GITHUB_SHA:0:12}-${GITHUB_RUN_ATTEMPT:-1}"

echo "::group::deploy ${name} (no traffic)"
gcloud run deploy "$name" --image "$image" --region "$region" --no-traffic --tag "$tag" --quiet
rev="$(gcloud run services describe "$name" --region "$region" --format='value(status.latestCreatedRevisionName)')"
echo "revision ${rev}"
echo "::endgroup::"

ready() {
	[ "$(gcloud run revisions describe "$rev" --region "$region" --format=json |
		jq -r '.status.conditions[] | select(.type == "Ready") | .status')" = "True" ]
}

if ! ready; then
	echo "rollout: revision ${rev} is not ready" >&2
	exit 1
fi

echo "::group::${name}: 10 % to ${rev}"
gcloud run services update-traffic "$name" --region "$region" --to-revisions "${rev}=10" --quiet
echo "::endgroup::"

sleep "$soak"

if ! ready; then
	echo "rollout: revision ${rev} became unready during the canary; rolling back" >&2
	gcloud run services update-traffic "$name" --region "$region" --to-revisions "${rev}=0" --quiet || true
	exit 1
fi

echo "::group::${name}: 100 % to latest"
gcloud run services update-traffic "$name" --region "$region" --to-latest --quiet
gcloud run services update "$name" --region "$region" --remove-tags "$tag" --quiet || true
echo "::endgroup::"
