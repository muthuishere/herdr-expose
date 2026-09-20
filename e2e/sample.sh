#!/usr/bin/env bash
# sample.sh <pid> <seconds> [interval]
# Prints RSS(MB) CPU% threads fds herdr-children for a herdr-expose process.
# The RSS number that matters is the LAST column: the server PLUS every
# `herdr terminal session` subprocess it spawned, because that is what the
# machine actually pays.
pid="$1"; secs="${2:-20}"; iv="${3:-2}"
end=$(( $(date +%s) + secs ))
printf 't\trss_mb\tcpu\tthreads\tfds\tkids\tkids_rss_mb\ttotal_mb\n'
t0=$(date +%s)
while [ "$(date +%s)" -lt "$end" ]; do
  read -r rss cpu <<<"$(ps -o rss=,%cpu= -p "$pid" 2>/dev/null)"
  [ -z "$rss" ] && break
  th=$(ps -M "$pid" 2>/dev/null | tail -n +2 | wc -l | tr -d ' ')
  fds=$(lsof -p "$pid" 2>/dev/null | wc -l | tr -d ' ')
  kidpids=$(pgrep -P "$pid" 2>/dev/null | tr '\n' ' ')
  kn=0; krss=0
  for k in $kidpids; do
    kr=$(ps -o rss= -p "$k" 2>/dev/null | tr -d ' ')
    [ -n "$kr" ] && { kn=$((kn+1)); krss=$((krss+kr)); }
  done
  printf '%d\t%.1f\t%s\t%s\t%s\t%s\t%.1f\t%.1f\n' \
    $(( $(date +%s) - t0 )) "$(echo "$rss/1024"|bc -l)" "$cpu" "$th" "$fds" "$kn" \
    "$(echo "$krss/1024"|bc -l)" "$(echo "($rss+$krss)/1024"|bc -l)"
  sleep "$iv"
done
