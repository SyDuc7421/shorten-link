# Small helper packages

Three packages too small for a page of their own.

## `internal/ctxkey`: who is the caller?

Stores and reads the authenticated user's ID in a request's `context.Context`.

```go
ctx = ctxkey.SetUserID(ctx, 42)   // middleware.JWTAuth, after a valid token
id, ok := ctxkey.UserID(ctx)      // handlers; ok is false for anonymous requests
```

The key is an unexported struct type, so no other package can read or
overwrite the value by accident with a plain string key. Always check `ok`:
the zero ID is not a valid user.

## `internal/logger`: structured logs

- `logger.New()` returns a zap production logger: JSON lines at info level and
  above, written to stderr.
- `logger.WithContext(ctx, l)` and `logger.FromContext(ctx)` carry a logger in
  the request context. `middleware.RequestID` stores one tagged with the
  request ID. `FromContext` falls back to zap's global logger when none is set.

Today `cmd/server` and `cmd/worker` pass the base logger down explicitly
(publisher, consumers), and `FromContext` is not called anywhere yet.

## `api`: the embedded OpenAPI spec

`api/openapi.yaml` is the API reference, written by hand. `api/doc.go` embeds it
into the binary with `//go:embed`, and `handler.RegisterDocs` serves it at
`/api/openapi.yaml` and as Swagger UI at `/docs`.

Two consequences:

- Changing a handler does **not** update the docs. Edit the YAML in the same
  commit.
- Because the file is compiled in, restart the server to see a change at `/docs`.

To check the file after editing it, any OpenAPI 3.0 validator works, for example
`openapi-spec-validator api/openapi.yaml` (Python package of the same name).
