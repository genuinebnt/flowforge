#!/bin/sh
# Creates the spec 11.7 topics, dead-letter topics and one subscription per
# consumer in the Pub/Sub emulator, mirroring infra/modules/pubsub-topic.
# Used by deploy/docker-compose.yml and .github/workflows/ci.yml.
set -eu

host="${PUBSUB_EMULATOR_HOST:-localhost:8085}"
project="${FF_PUBSUB_PROJECT:-ff-local}"
prefix="${FF_TOPIC_PREFIX:-ff.}"
base="http://${host}/v1/projects/${project}"

put() {
	# 409 (already exists) is fine: the script is idempotent.
	code=$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H 'Content-Type: application/json' -d "$2" "$1")
	case "$code" in
	200 | 409) ;;
	*) echo "PUT $1 failed: HTTP $code" >&2; exit 1 ;;
	esac
}

topic() {
	put "${base}/topics/${prefix}$1" '{}'
	put "${base}/topics/${prefix}$1.dead-letter" '{}'
}

sub() {
	t="${prefix}$1"
	put "${base}/subscriptions/${t}.$2" "{
		\"topic\": \"projects/${project}/topics/${t}\",
		\"ackDeadlineSeconds\": 60,
		\"enableMessageOrdering\": true,
		\"retryPolicy\": {\"minimumBackoff\": \"10s\", \"maximumBackoff\": \"600s\"},
		\"deadLetterPolicy\": {\"deadLetterTopic\": \"projects/${project}/topics/${t}.dead-letter\", \"maxDeliveryAttempts\": 10}
	}"
}

for t in run.events approval.events graph.changes scope.events auth.events; do
	topic "$t"
done

sub run.events notifier
sub run.events gateway
sub approval.events notifier
sub graph.changes notifier
sub graph.changes engine
sub graph.changes similarity
sub scope.events engine
sub scope.events tool-runner
sub scope.events egress-proxy
sub scope.events notifier
sub auth.events notifier

echo "pubsub emulator ready at ${host} (project ${project}, prefix ${prefix})"
