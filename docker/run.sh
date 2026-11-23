#!/bin/sh
# Container entrypoint. Runs the bridge and passes its exit code on, except when Beeper rejects the
# appservice registration (mautrix exit codes 16/17, e.g. after the bridge was deleted in Beeper).
# Restarting can't fix that, only a new `bbctl config`, so instead of exiting into a restart loop
# this waits until /data/config.yaml changes and then starts the bridge again. Meanwhile the
# healthcheck reports the container as unhealthy.
CONFIG=/data/config.yaml
FLAG=/tmp/needs-bbctl-config
pid=
stopping=
trap 'stopping=1; [ -n "$pid" ] && kill -TERM "$pid" 2>/dev/null' TERM INT

while :; do
	rm -f "$FLAG"
	/usr/bin/mautrix-rift -c "$CONFIG" --no-update &
	pid=$!
	wait "$pid"
	code=$?
	# wait returns early when a signal arrives; keep waiting until the bridge has really exited.
	while kill -0 "$pid" 2>/dev/null; do
		wait "$pid"
		code=$?
	done
	pid=
	[ -n "$stopping" ] && exit "$code"
	case "$code" in
	16 | 17) ;;
	*) exit "$code" ;;
	esac

	touch "$FLAG"
	echo "Beeper rejected the bridge registration (exit code $code). Was the bridge deleted in Beeper?"
	echo "Fix: run 'bbctl config --type bridgev2 -o config.yaml <bridge name>', apply your settings, and"
	echo "replace $CONFIG. The bridge restarts by itself once the file changes (checked every 10 s)."
	before=$(md5sum "$CONFIG" 2>/dev/null)
	while [ "$(md5sum "$CONFIG" 2>/dev/null)" = "$before" ]; do
		sleep 10 &
		wait $!
		[ -n "$stopping" ] && exit 0
	done
	echo "$CONFIG changed, starting the bridge again"
done
