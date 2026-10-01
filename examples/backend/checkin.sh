#!/bin/sh
# Send one scheduled-job check-in to Flare.
#
# The URL is the project-specific URL shown by Flare after a monitor has been
# configured. It contains the project's public ingest key. Keep it in a secret
# store or protected job variable and do not print it in CI logs.
set -eu

checkin_url=${FLARE_CHECKIN_URL:?set FLARE_CHECKIN_URL to the project check-in URL}
status=${1:-ok}

case "$status" in
ok)
	;;
error)
	case "$checkin_url" in
		*\?*) checkin_url="${checkin_url}&status=error" ;;
		*) checkin_url="${checkin_url}?status=error" ;;
	esac
	;;
*)
	echo "usage: $0 [ok|error]" >&2
	exit 2
	;;
esac

# curl retries transient HTTP failures and transport errors, but does not retry
# a permanent 4xx payload. The bounded timeout keeps a stuck network from
# holding a scheduler slot forever. A 2xx response is still only an accepted
# check-in; monitor state should be verified in Flare during rollout.
curl --silent --show-error --fail \
	--connect-timeout 5 --max-time 20 \
	--retry 3 --retry-delay 1 --retry-max-time 20 \
	"$checkin_url" >/dev/null
