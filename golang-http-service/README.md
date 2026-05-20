# golang-http-service

A small Go HTTP service built following the patterns from Mat Ryer's post
"How I write HTTP services in Go after 13 years"
(https://grafana.com/blog/2024/02/09/how-i-write-http-services-in-go-after-13-years/).

The point of this project is to explore the patterns themselves, not to
ship a particular feature. The example domain is a trivial in-memory
"widgets" CRUD-ish API.

## Patterns demonstrated

- **`NewServer` constructor** in `internal/server/server.go` returns a
  single `http.Handler` and takes every dependency as an argument. Routes
  live in `routes.go`; the constructor applies cross-cutting middleware.
- **`func main()` only calls `Run`.** `Run(ctx, args, getenv, stdin,
  stdout, stderr) error` takes the OS fundamentals as arguments so tests
  can drive the same entry point with controlled inputs. `cmd/server/main.go`
  is the five-line shell.
- **`signal.NotifyContext` lives inside `Run`** (not in `main`) so its
  `cancel` actually runs — per the explicit EDIT note in the article.
- **Graceful shutdown** via the cancelled context + `http.Server.Shutdown`.
- **Maker funcs return the handler.** Handlers are factory functions
  returning `http.Handler`, e.g. `handleCreateWidget(logger, widgets)`,
  with per-handler setup and inline request/response types living in the
  closure.
- **Generic `encode` / `decode` / `decodeValid` helpers** in `encoding.go`.
  `encode[T]` takes `(w, r, status, v)` (the `r` is kept for future
  content negotiation). `decode[T]` is the plain JSON path; `decodeValid[T
  Validator]` constrains `T` to types that implement the `Validator`
  interface and rejects values whose `Valid` returns problems.
- **`Validator` single-method interface** returning `map[string]string`
  of field → human-readable explanation. The 422 response surfaces the
  same map under `problems`.
- **`sync.Once` for deferred per-handler setup.** `handleHello` parses
  its template lazily on first request and reuses it; init errors are
  captured outside the `Do` block and surfaced on every call.
- **Adapter-style middleware** as plain `http.Handler` wrappers
  (`withRequestLogging`, `withRecover`).
- **Go 1.22+ ServeMux** for method+path routing (`GET /api/widgets/{id}`)
  — no third-party router.
- **End-to-end testing via `Run`.** `server_test.go` boots the real
  binary on a random port, polls a `waitForReady(ctx, timeout, endpoint)`
  helper, and exercises the HTTP surface like a real user. `t.Cleanup`
  cancels the context and verifies graceful shutdown.

## Layout

```
golang-http-service/
  justfile
  go.mod
  README.md
  cmd/
    server/
      main.go              # signal.NotifyContext + server.Run
  internal/
    server/                # one flat package — all HTTP code
      server.go            # NewServer
      run.go               # Run(ctx, args, getenv, stdout, stderr)
      routes.go            # addRoutes(mux, deps...)
      handlers.go          # handleX factory functions
      middleware.go        # withRequestLogging, withRecover
      encoding.go          # encode/decode generics + Validator
      server_test.go       # end-to-end test driving Run()
    store/
      store.go             # Store interface + MemoryStore
```

Idiomatic Go: `cmd/<binary>/` for entry points and `internal/` for the
language-enforced private packages. Inside `internal/server` the
package is kept flat — splitting `handlers`, `middleware`, `routes`
into separate packages would create import ceremony without buying
isolation (they all share types and reference each other).

`internal/store` is lifted out because it has a clean interface
boundary: handlers depend on `store.Store`, not on the concrete
`MemoryStore`. A future `postgres.go` or `redis.go` would slot in
without touching HTTP code.

## Running

Via `just`:

```sh
just run                # listens on 127.0.0.1:8080
HOST=0.0.0.0 PORT=9000 just run
just build              # binary at ./bin/server
just test               # go test ./...
just check              # fmt + vet + test
just --list             # all recipes
```

Or directly:

```sh
go run ./cmd/server
go test ./...
```

## Endpoints

| Method | Path                | Body                          | Response |
| ------ | ------------------- | ----------------------------- | -------- |
| GET    | `/healthz`          | -                             | `{"status":"ok"}` |
| GET    | `/hello/{name}`     | -                             | HTML rendered from a `sync.Once`-parsed template |
| GET    | `/api/widgets`      | -                             | `{"widgets":[...]}` |
| GET    | `/api/widgets/{id}` | -                             | `Widget` or 404 |
| POST   | `/api/widgets`      | `{"name":"...","price":123}`  | 201 with widget, or 422 with `problems` |

The justfile includes curl helpers against a running server:

```sh
just healthz
just create-widget sprocket 42
just list-widgets
just get-widget 1
```
