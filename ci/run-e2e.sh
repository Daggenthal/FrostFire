#!/bin/sh
# Runs ci/e2e.sh, streaming its output. On failure, the last part of the log is also
# posted as a GitHub annotation, so it can be read without downloading the job log.
log=/tmp/frostfire-e2e.log
{ timeout -s TERM 2700 sh "$(dirname "$0")/e2e.sh"; echo $? > /tmp/frostfire-e2e.rc; } 2>&1 | tee "$log"
rc=$(cat /tmp/frostfire-e2e.rc)
if [ "$rc" != 0 ]; then
    printf '::error title=e2e failed on %s::' "$(uname -s)"
    tail -c 3500 "$log" | awk 'BEGIN { ORS = "%0A" } { gsub(/%/, "%25"); gsub(/\r/, ""); print }'
    echo
fi
exit "$rc"
