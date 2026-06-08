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
- **Two middleware styles, side by side:**
  - *Simple adapter* — `adminOnly(next http.Handler) http.Handler` is the
    article's minimal form: no deps beyond `next`. It reads the user from
    context (placed there by the auth middleware) and 404s non-admins so
    routes don't even leak their existence.
  - *Factory returning middleware* — `newAuthMiddleware(logger, auther)
    func(http.Handler) http.Handler` binds dependencies once and returns
    the wrapper. Routes.go calls `requireAuth := newAuthMiddleware(...)`
    once and then wraps each handler with `requireAuth(...)`, instead of
    repeating the dep list at every route. This is the pattern to reach
    for whenever middleware needs more than just `next`.
- **`withRequestLogging` / `withRecover`** stay as plain
  `(logger, next) -> http.Handler` wrappers — applied once at the top of
  `NewServer`, so the factory form would buy nothing.
- **Per-request logging context (`internal/logging`)** — `ContextHandler`
  wraps the base `slog.Handler` and merges a per-request *attribute bag*
  (a `*struct{ mu sync.Mutex; attrs []slog.Attr }` stored in `ctx`) into
  every record. `withRequestID` seeds the bag with `request_id` (echoed
  on the `X-Request-Id` response header), `newAuthMiddleware` adds
  `user_id` and `admin` after a successful authenticate, and handlers
  use `logger.InfoContext(r.Context(), ...)` so every log line is
  tagged. The bag is mutable on purpose: `withRequestLogging` emits its
  log line *after* the inner handler returns, so attrs added by auth
  appear on the request log line too.
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
      main.go              # tiny: calls server.Run
  internal/
    server/                # one flat package — all HTTP code
      server.go            # NewServer
      run.go               # Run(ctx, args, getenv, stdin, stdout, stderr)
      routes.go            # addRoutes(mux, deps...)
      handlers.go          # handleX factory functions
      middleware.go        # withRequestLogging, withRecover,
                           # newAuthMiddleware (factory), adminOnly (adapter)
      encoding.go          # encode/decode/decodeValid + Validator
      server_test.go       # end-to-end test driving Run()
    store/
      store.go             # Store interface + MemoryStore
    auth/
      auth.go              # User, Authenticator interface,
                           # context helpers, StaticAuthenticator demo
    logging/
      logging.go           # ContextHandler + per-request attr bag,
                           # NewLogger wraps slog.NewTextHandler
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

| Method | Path                  | Auth         | Body                          | Response |
| ------ | --------------------- | ------------ | ----------------------------- | -------- |
| GET    | `/healthz`            | public       | -                             | `{"status":"ok"}` |
| GET    | `/hello/{name}`       | public       | -                             | HTML rendered from a `sync.Once`-parsed template |
| GET    | `/api/widgets`        | Bearer       | -                             | `{"widgets":[...]}` |
| GET    | `/api/widgets/{id}`   | Bearer       | -                             | `Widget` or 404 |
| POST   | `/api/widgets`        | Bearer       | `{"name":"...","price":123}`  | 201 with widget, or 422 with `problems` |
| DELETE | `/api/widgets/{id}`   | Bearer + admin | -                           | 204 on success, 404 otherwise |

Demo tokens (hard-coded in `Run` via `auth.NewStatic`):

```
Authorization: Bearer demo-user-token     # ordinary user
Authorization: Bearer demo-admin-token    # admin
```

Every response carries an `X-Request-Id` header (echoed if the client
supplied one, generated otherwise). Server logs use `slog` text format
with the per-request bag attributes merged in:

```
time=… level=INFO msg=request method=GET   path=/healthz       status=200 duration=39µs   request_id=23763a9a350c4f90
time=… level=INFO msg=request method=POST  path=/api/widgets   status=201 duration=174µs  request_id=demo-rid-001 user_id=u2 admin=true
time=… level=INFO msg=request method=GET   path=/api/widgets   status=200 duration=58µs   request_id=134ad3d14e36dc76 user_id=u1 admin=false
```

Any log emitted from a handler or downstream code via
`logger.InfoContext(r.Context(), …)` (or `slog.InfoContext(ctx, …)`)
picks up the same `request_id` / `user_id`, so a single request's log
lines can be grepped together.

The justfile includes curl helpers against a running server (tokens
default to the demo values; override with `USER_TOKEN` / `ADMIN_TOKEN`):

```sh
just healthz
just hello sasha
just create-widget sprocket 42
just list-widgets
just get-widget 1
just delete-widget 1            # uses ADMIN_TOKEN
```
