# golang-http-service

A small Go HTTP service built following the patterns from Mat Ryer's post
"How I write HTTP services in Go after 13 years"
(https://grafana.com/blog/2024/02/09/how-i-write-http-services-in-go-after-13-years/).

The point of this project is to explore the patterns themselves, not to
ship a particular feature. The example domain is a trivial in-memory
"widgets" CRUD-ish API.

## Patterns demonstrated

- `NewServer` constructor in `internal/server/server.go` returns a single
  `http.Handler` and wires every dependency. Routes live in `routes.go`;
  the constructor applies cross-cutting middleware.
- `Run(ctx, args, getenv, stdout, stderr) error` in `internal/server/run.go`
  — `cmd/server/main.go` is a five-line shell that calls it. The whole
  program is trivially testable end-to-end.
- Graceful shutdown via `signal.NotifyContext` + `http.Server.Shutdown`.
- Handlers are factory functions returning `http.Handler`, e.g.
  `handleCreateWidget(logger, widgets)`. Per-handler setup (closures over
  dependencies, response types declared inline) lives next to the request
  logic.
- Generic `encode[T]` / `decode[T]` helpers in `encoding.go`. `decode`
  automatically calls a `Validator` interface if the request type
  implements it, returning a `map[string]string` of problems for 422
  responses.
- Middleware are plain `http.Handler` wrappers (`withRequestLogging`,
  `withRecover`).
- `net/http`'s Go 1.22+ ServeMux is used for method+path routing
  (`GET /api/widgets/{id}`) — no third-party router needed.
- `server_test.go` boots the real binary via `Run()` on a random port and
  exercises it over real HTTP, using a `waitForReady` poll.

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
