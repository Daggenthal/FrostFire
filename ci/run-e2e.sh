#!/bin/sh
# Runs ci/e2e.sh with an overall time limit. Output goes to a file rather than a pipe,
# so a stuck child process can't keep the run alive after the limit. The log is then
# printed, and on failure its tail is also posted as a GitHub annotation, so it can be
# read without downloading the job log.
log=/tmp/frostfire-e2e.log
timeout -k 30 2700 sh "$(dirname "$0")/e2e.sh" > "$log" 2>&1
rc=$?
cat "$log"
if [ "$rc" != 0 ]; then
    printf '::error title=e2e failed on %s (exit %s)::' "$(uname -s)" "$rc"
    tail -c 3500 "$log" | awk 'BEGIN { ORS = "%0A" } { gsub(/%/, "%25"); gsub(/\r/, ""); print }'
    echo
fi
exit "$rc"
