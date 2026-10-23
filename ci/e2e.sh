#!/bin/sh
# End-to-end test on a throwaway machine (CI runner or VM), run as root:
# setup, init, up, check the containers can reach each other, back up,
# break the site and the database, restore, and check everything came back.
set -eux

ROOT=/srv/frostfire
ENGINE="${FROSTFIRE_ENGINE:-podman}"

# Usage: compose SECONDS ARGS...  (a time limit, so a stuck engine fails the test instead of hanging it)
compose() {
    secs=$1; shift
    timeout "$secs" "$ENGINE" compose --project-name frostfire --file "$ROOT/stack/compose.yaml" --env-file "$ROOT/frostfire.env" "$@"
}

dump_logs() {
    set +e
    timeout 60 "$ENGINE" ps -a
    compose 120 logs --tail 60
}
trap 'test $? -eq 0 || dump_logs' EXIT
trap 'echo "e2e: timed out"; exit 124' TERM

# Fail instead of hanging: every container command gets a time limit.
t() { timeout "$@"; }

# Checkpoints show up as annotations on the CI run.
checkpoint() { echo "::notice title=e2e $(uname -s)::$*"; }

# --- prerequisites, twice: the second run must be a no-op
frostfire setup --dry-run
frostfire setup --yes
frostfire setup --yes | tee /tmp/setup-again.txt
grep -q "Everything is already in place" /tmp/setup-again.txt
checkpoint "setup ok, second run was a no-op"

# --- install
frostfire init
mkdir -p /var/backups
cat >> "$ROOT/frostfire.env" <<EOF
DOMAIN=ci.example.org
ADMIN_EMAIL=ci@example.org
SMTP_HOST=
RESTIC_REPOSITORY=/var/backups/frostfire-ci
EOF
[ -n "${FROSTFIRE_ENGINE:-}" ] && echo "FROSTFIRE_ENGINE=$FROSTFIRE_ENGINE" >> "$ROOT/frostfire.env"
frostfire repo init
checkpoint "init and repo init ok"

cat > "$ROOT/www/index.php" <<'EOF'
<?php
foreach (['db' => 3306, 'cache' => 11211] as $host => $port) {
    $c = @fsockopen($host, $port, $errno, $errstr, 5);
    echo $host, ': ', $c ? 'reachable' : "unreachable ($errstr)", "\n";
}
echo 'mysqli: ', extension_loaded('mysqli') ? 'yes' : 'no', "\n";
echo 'memcached: ', extension_loaded('memcached') ? 'yes' : 'no', "\n";
EOF

t 1800 frostfire up
checkpoint "frostfire up finished"
compose 60 ps

# --- wait for MariaDB (first start initializes the datadir)
i=0
until t 60 frostfire db dump /tmp/probe.sql 2>/dev/null; do
    i=$((i + 1))
    [ "$i" -gt 60 ] && exit 1
    sleep 5
done

# --- containers reach each other by name, PHP has its extensions
compose 60 exec -T php php /srv/www/index.php > /tmp/php.txt
cat /tmp/php.txt
grep -q "db: reachable" /tmp/php.txt
grep -q "cache: reachable" /tmp/php.txt
grep -q "mysqli: yes" /tmp/php.txt
grep -q "memcached: yes" /tmp/php.txt
checkpoint "stack up, PHP reaches db and cache by name"

# --- nginx answers on port 80 (bootstrap mode: 503 until a cert exists)
case "$(uname -s)" in
    FreeBSD) ip=$(ifconfig | awk '/inet / && $2 != "127.0.0.1" { print $2; exit }') ;;
    *)       ip=127.0.0.1 ;;
esac
i=0
until [ "$(curl -s -m 5 -o /dev/null -w '%{http_code}' "http://$ip/")" = "503" ]; do
    i=$((i + 1))
    [ "$i" -gt 30 ] && { curl -sv "http://$ip/" || true; exit 1; }
    sleep 2
done
curl -s -m 10 -H 'Host: ci.example.org' "http://$ip/.well-known/acme-challenge/missing" -o /dev/null -w '%{http_code}\n' | grep -q 404
checkpoint "nginx answering on port 80"

# --- data to protect
printf 'CREATE DATABASE ci_marker;\nCREATE TABLE ci_marker.t (v VARCHAR(20));\nINSERT INTO ci_marker.t VALUES ("frostfire-ok");\n' > /tmp/seed.sql
t 900 frostfire db load /tmp/seed.sql --yes

# --- back up, then break things
t 900 frostfire backup
frostfire list
echo broken > "$ROOT/www/index.php"
echo 'DROP DATABASE ci_marker;' > /tmp/drop.sql
t 900 frostfire db load /tmp/drop.sql --yes
t 900 frostfire db dump /tmp/broken.sql
! grep -q frostfire-ok /tmp/broken.sql
checkpoint "backup done, site and database broken on purpose"

# --- restore and check
t 900 frostfire restore latest --yes
grep -q fsockopen "$ROOT/www/index.php"
i=0
until t 60 frostfire db dump /tmp/restored.sql 2>/dev/null; do
    i=$((i + 1)); [ "$i" -gt 30 ] && exit 1; sleep 5
done
grep -q frostfire-ok /tmp/restored.sql
checkpoint "restore brought back files and database"

frostfire prune
frostfire repo check
t 120 frostfire doctor || true   # the only expected failure is the missing TLS certificate
checkpoint "E2E PASSED on $(uname -sr)"
