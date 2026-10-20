#!/usr/bin/env bash
# Benchmarks sidecar models on the blueprint compaction cases.
# Usage: [ENDPOINT=url] [API_KEY_ENV=HF_TOKEN] [RUNS=3] [REASONING_EFFORT=none] [EXTRA_BODY='{json}'] ./run.sh model [model...]
# Writes out/<model>/<case>.<run>.{json,md} and appends out/times.tsv.
set -euo pipefail
cd "$(dirname "$0")"
ENDPOINT=${ENDPOINT:-http://localhost:11434/v1/chat/completions}
ENDPOINT=${ENDPOINT%/}; [[ $ENDPOINT == */v1 ]] && ENDPOINT+=/chat/completions
RUNS=${RUNS:-3}
[[ -z ${EXTRA_BODY:-} ]] && EXTRA_BODY='{}'
# Same cap field as internal/llm: Ollama ignores max_completion_tokens.
CAP=max_tokens; [[ $ENDPOINT == *api.openai.com* || $ENDPOINT == *.openai.azure.com* ]] && CAP=max_completion_tokens
auth=()
[[ -n ${API_KEY_ENV:-} ]] && auth=(-H "Authorization: Bearer ${!API_KEY_ENV}")

# Verbatim from internal/llm/llm.go; keep in sync.
DIFF_SYS="You summarize code diffs. Reply with a file-by-file change list: one line per file (path — what changed), then 1-3 sentences on overall intent. No code blocks unless a 1-2 line snippet is essential. Keep it under 250 words."
THREAD_SYS="You write handoff briefs for coding agents joining mid-session. Reply with: 1) What was decided (bullets), 2) What each agent is doing (agent: task), 3) Open questions and blockers. Under 300 words. No preamble."

call() { # model system userfile outbase
  jq -n --arg m "$1" --arg s "$2" --rawfile u "$3" --arg r "${REASONING_EFFORT:-}" --arg cap "$CAP" --argjson x "$EXTRA_BODY" \
    '$x + {model:$m,messages:[{role:"system",content:$s},{role:"user",content:$u}],temperature:0.2,($cap):1000}
     + (if $r == "" then {} else {reasoning_effort:$r} end)' |
    curl -sS --fail-with-body --max-time 120 ${auth[@]+"${auth[@]}"} -H 'Content-Type: application/json' \
      -d @- -o "$4.json" -w '%{time_total}' "$ENDPOINT"
}

mkdir -p out
for model in "$@"; do
  dir="out/${model//[:\/]/_}"; mkdir -p "$dir"
  call "$model" "$DIFF_SYS" cases/d1_clean_phase.txt "$dir/warmup" >/dev/null || { # load weights; untimed
    echo "$model: request to $ENDPOINT failed, skipping: $(cat "$dir/warmup.json")" >&2; continue; }
  for f in cases/*.txt; do
    c=$(basename "$f" .txt)
    sys=$THREAD_SYS; [[ $c == d* ]] && sys=$DIFF_SYS
    for i in $(seq "$RUNS"); do
      t=$(call "$model" "$sys" "$f" "$dir/$c.$i" || echo ERR)
      jq -r '.choices[0].message.content // .error // .' "$dir/$c.$i.json" > "$dir/$c.$i.md" 2>/dev/null || true
      fin=$(jq -r '.choices[0].finish_reason // "?"' "$dir/$c.$i.json" 2>/dev/null || echo "?")
      tok=$(jq -r '.usage.completion_tokens // "?"' "$dir/$c.$i.json" 2>/dev/null || echo "?")
      printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$model" "$c" "$i" "$t" "$fin" "$tok" | tee -a out/times.tsv
    done
  done
  rm -f "$dir/warmup.json"
done
