# leak-demo

The API behind [pigfox.com/demos/goroutine-leaks](https://pigfox.com/demos/goroutine-leaks),
served at leaks.pigfox.com. Visitors start real goroutine leaks and watch the
goroutine leak profile that Go 1.27 turns on by default find them.

## How it works

- internal/scenarios: five leaks (send with no receiver, forgotten wg.Done,
  mutex never unlocked, select that can never fire, context never cancelled),
  each with a fixed twin, plus one honest blind spot (a goroutine stuck on a
  channel that is still reachable from a global, which the profile cannot
  report) and a healthy idle pool. The page shows the demo:begin blocks.
- internal/worker: the child process that holds the leaks. A leaked goroutine
  never ends, so the only way to give its memory back is to end the process.
  Each profile read runs the leak check twice: one pass can miss a few leaks.
- internal/server: the public server. It starts the worker, replaces it every
  RESET_EVERY (10m) or when it dies, caps leaks per worker (LEAK_CAP, 2000),
  rate-limits per IP (20 runs a minute, 1 state read a second), caches the
  profile for 3 s, and never exposes /debug/pprof.

## API

    GET  /api/scenarios   bugs, fixes, code snippets (static)
    GET  /api/state       {"status": {...}, "profile": {"leaks": ..., "dump": ...}}
    POST /api/run         {"scenario": "send", "variant": "leaky" | "fixed"}

## Run

    go test -race ./...
    go run ./cmd/leak-demo        # :8549

Deploy on 104.223.116.10:

    VERSION=<sha> CREATED_AT=<UTC ISO> docker compose up -d --build
