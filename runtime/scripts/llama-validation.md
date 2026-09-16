# Real llama.cpp validation

Validated on 2026-09-16, macOS arm64, using actual CPU model inference.

- Server: Homebrew `llama.cpp 0.4.1`, build 10964, commit `b29c606e2`.
- Model: `ggml-org/models`, `tinyllamas/stories15M-q4_0.gguf` (18.1 MiB).
- Download: `https://huggingface.co/ggml-org/models/resolve/main/tinyllamas/stories15M-q4_0.gguf`.
- SHA256: `66967fbece6dbe97886593fdbb73589584927e29119ec31f08090732d1861739`.
- Local reusable model: `kernel/.build/stories15M-q4_0.gguf` (ignored build artifact).

From the repository root, run the server and probe in separate terminals:

```sh
llama-server -m kernel/.build/stories15M-q4_0.gguf --host 127.0.0.1 --port 18081 -c 128 -np 1 -ngl 0 --no-webui
python3 runtime/scripts/llama-smoke.py --require-cache
```

The probe builds `aok-engine-llama`, hands it a Unix listener, connects through
the existing JSON-RPC Engine protocol, creates a session, performs two identical
completion requests, aborts a third request, closes the session, and verifies
clean SIGTERM shutdown. It never substitutes echo output.

Observed after a fresh server start:

```json
{
  "check": "AOK_LLAMA_REAL_SMOKE",
  "status": "pass",
  "provider": "llama.cpp",
  "turns": [
    {"usage": {"input_tokens": 27, "output_tokens": 32, "cached_tokens": 0}},
    {"usage": {"input_tokens": 27, "output_tokens": 32, "cached_tokens": 26}}
  ],
  "abort": "cancelled"
}
```

Both completed turns generated:

```text
 a big, round pumpkin in the garden. She was so excited and wanted to show it to her mom.
Lily ran inside and said,
```

Cache accounting uses `timings.cache_n`. In the tested server, `tokens_cached`
was 58 after a 27-input/32-output completion; that field describes occupied KV
cache and would overstate the number of reused input tokens. A cached request
reported `timings.cache_n=26` and `timings.prompt_n=1`. The provider checks their
sum against `tokens_evaluated` and fails when cache-enabled responses omit the
measured cache count.

`go test -race ./... -run TestLlama` and `go vet ./...` passed. Provider-specific tests cover
measured usage, cache consistency, invalid/negative/fractional/overflow counters,
missing usage, backend errors, redirects, response and prompt size limits,
context cancellation, and deadline expiry. No credentials are needed for this
loopback verification; backend error bodies are not forwarded.

This validates the host HTTP provider and the existing userspace Engine path.
It does not validate `/dev/ainf`, guest-to-host vsock, kernel token budgets,
Application-owned KV handles, prefill/decode separation, or kernel-enforced
capability checks. The tiny model validates execution and accounting, not
agent-quality reasoning. HTTP cancellation terminates the caller's request;
immediate interruption of backend compute and accounting for a cancelled
request's partial token consumption remain unproven. Successful completions
return measured token counts; cancelled/failed HTTP requests have no trustworthy
usage response. The temporary server was stopped after validation.
