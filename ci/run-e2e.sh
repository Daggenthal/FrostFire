#!/bin/sh
# Runs ci/e2e.sh with an overall time limit. Output goes to a file rather than a pipe,
# so a stuck child process can't keep the run alive after the limit. The log is then
# printed, and on failure its tail is also posted as a GitHub annotation, so it can be
# read without downloading the job log.
log=/tmp/frostfire-e2e.log
timeout -k 120 6000 sh "$(dirname "$0")/e2e.sh" > "$log" 2>&1
rc=$?
cat "$log"
if [ "$rc" != 0 ]; then
    # The last 12 KB, as four annotations of 3 KB each.
    tail -c 12000 "$log" > /tmp/frostfire-e2e.tail
    for part in 1 2 3 4; do
        printf '::error title=e2e failed on %s (exit %s) part %s of 4::' "$(uname -s)" "$rc" "$part"
        dd if=/tmp/frostfire-e2e.tail bs=3000 skip=$((part - 1)) count=1 2>/dev/null |
            awk 'BEGIN { ORS = "%0A" } { gsub(/%/, "%25"); gsub(/\r/, ""); print }'
        echo
    done
fi
exit "$rc"
