# seqq: read-only, token-lean Seq queries through seqcli.
#   seqq s "<filter>" [count=20] [since=1h] [until]   events: one line each, exception trimmed to 2 lines
#   seqq q "<sql>" [since=1h] [until]                 server-side SQL aggregate, CSV rows
#   seqq raw "<filter>" [count=5] [since=1h]          full native events (only when the detail is needed)
# since/until: ISO 8601 (2026-11-24T13:00Z) or relative 30m / 6h / 2d.
# Server and key come from the environment (SEQ_URL, SEQ_READONLY_KEY: the
# directory's ambient credentials); the key goes to seqcli by environment,
# never argv.
set -euo pipefail

usage() { grep '^#   seqq' "$0" >&2; exit 2; }
: "${SEQ_URL:?SEQ_URL not set (run from a directory whose .envrc provides it)}"
: "${SEQ_READONLY_KEY:?SEQ_READONLY_KEY not set (an ambient credential)}"
export SEQCLI_CONNECTION_SERVERURL=$SEQ_URL SEQCLI_CONNECTION_APIKEY=$SEQ_READONLY_KEY

ts() {  # relative (30m/6h/2d) -> ISO UTC; ISO passes through
  case "$1" in
    *[0-9]m) date -u -d "-${1%m} minutes" +%FT%TZ ;;
    *[0-9]h) date -u -d "-${1%h} hours" +%FT%TZ ;;
    *[0-9]d) date -u -d "-${1%d} days" +%FT%TZ ;;
    *) printf '%s' "$1" ;;
  esac
}

[ "$#" -ge 2 ] || usage
cmd=$1; expr=$2
case "$cmd" in
  s)
    count=${3:-20}; start=$(ts "${4:-1h}"); end=()
    [ -n "${5:-}" ] && end=(--end="$(ts "$5")")
    seqcli search -f "$expr" -c "$count" --start="$start" "${end[@]}" --no-websockets --no-color \
      --column "coalesce(ApplicationName, Application)" 2>&1 |
      awk '/^\[[0-9]{4}-/ { n=0; print substr($0,1,300); next }
           /^ *at / { next }
           { if (++n <= 2) print "    " substr($0,1,200) }'
    ;;
  q)
    start=$(ts "${3:-1h}"); end=()
    [ -n "${4:-}" ] && end=(--end="$(ts "$4")")
    seqcli query -q "$expr" --start="$start" "${end[@]}" --no-color --timeout=110000 2>&1 | head -200
    ;;
  raw)
    count=${3:-5}; start=$(ts "${4:-1h}")
    seqcli search -f "$expr" -c "$count" --start="$start" --no-websockets --native 2>&1 | cut -c1-2000
    ;;
  *) usage ;;
esac
