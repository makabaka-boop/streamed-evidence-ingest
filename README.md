# Upload Service

A small, crash-safe HTTP upload service built from **Go**, **SQLite** and a
local file directory.

An upload is a single `multipart/form-data` request containing **exactly one
JSON metadata part** and **exactly one file part** of at most **16 MiB**.
Bytes are streamed to a private staging file while a SHA-256 is computed
in flight — the whole file is never held in memory, and the client-supplied
filename is never used as a disk path. A blob becomes readable only **after**
it has been atomically renamed into a content-addressed location **and** its
metadata row has committed in SQLite. On startup the service sweeps files
left by pre-commit crashes but never removes an already-committed blob.

## Acceptance

The fixed acceptance sequence is:

```sh
docker compose config --quiet
docker compose build
docker compose run --rm verify
```

The last command starts the one-shot `verify` service, waits for the app to be
healthy, exercises the full HTTP contract and the crash/recovery paths, and
exits non-zero on the first violation. On success it prints
`VERIFY OK: all checks passed`.

The `verify` client namespace- and content-keys each run with a fresh random
token, so the command is **idempotent** and can be re-run against the same
data volume without collisions (use `docker compose down -v` if you also want
to discard the volume).

## Layout

```
cmd/uploadsrv/         long-lived HTTP service
cmd/verify/            one-shot acceptance client (healthcheck + full suite)
internal/multipartx/   strict, streaming multipart/form-data reader
internal/metadata/     strict JSON metadata decoder
internal/blobstore/    staging dir + atomic content-addressed publish + recovery
internal/store/        SQLite metadata store (the read gate)
internal/server/       HTTP handlers and failure-injection hooks
docker-compose.yml     app service + one-shot verify service
Dockerfile             static, distroless, non-root image
```

## HTTP contract

### Upload

```
POST /uploads
Content-Type: multipart/form-data; boundary=...

--...
Content-Disposition: form-data; name="metadata"
Content-Type: application/json

{"request_id":"client-supplied-id","media_type":"image/png"}
--...
Content-Disposition: form-data; name="file"; filename="anything.png"
Content-Type: image/png

<file bytes>
--...--
```

`request_id` is required (1–200 printable, non-space ASCII, no path
separators). `media_type` is optional.

| Result | Status |
|---|---|
| new blob committed | `201 Created` |
| same `request_id` **and** identical content (idempotent replay) | `200 OK`, original record |
| same `request_id`, different content | `409 Conflict` |
| duplicate/unknown/missing field, malformed JSON, unknown JSON key, duplicate JSON key, trailing JSON, truncated body | `400 Bad Request` |
| file > 16 MiB | `413 Payload Too Large` |

Success body:

```json
{
  "request_id": "client-supplied-id",
  "sha256": "…",
  "size": 1234,
  "media_type": "image/png",
  "created_at": "…",
  "download": "/blobs/client-supplied-id"
}
```

### Download

```
GET /blobs/{request_id}     -> 200 + bytes (supports Range), or 404
```

A request id is only resolvable once the corresponding database row exists,
so a renamed-but-uncommitted file is indistinguishable from one that never
existed.

### Health

```
GET /healthz -> 200 ok
```

## Why no half-finished product can ever be read

Publishing is a strict sequence; the database row is the sole read gate:

1. **stream** – bytes are copied in 32 KiB chunks from the request into a
   private `tmp/.upload-<random>.part` file and through a SHA-256 hasher.
   Nothing under the public blob directory is touched, and the full file is
   never resident in memory.
2. **fsync + rename** – the temp file is fsynced and atomically `rename(2)`d
   to `blobs/<sha[:2]>/<sha[2:]>`. The on-disk name is derived **only** from
   content; the client filename is discarded. A rename either happens or it
   doesn't — no partial name can appear.
3. **commit** – the metadata row is inserted in a SQLite transaction. This is
   the visibility gate: `GET` does a database lookup before opening any file.
4. **respond** – only now is the success response written.

Failure handling:

- A failure **before the rename** removes the private temp file.
- A failure **after the rename but before/within commit** leaves an *orphan*
  blob file with no database row. It cannot be read via the API and is reaped
  on the next startup. Live requests never delete blobs, so a request failing
  in this gap can never remove another request's committed, byte-identical
  file (distinct request ids may alias one content-addressed blob).
- A failure **after commit but before the response** leaves a fully committed,
  readable blob; the client simply retries and receives the original record.

### Startup recovery

Before the listener starts, the service queries all `stored_as` names from
SQLite and:

- deletes everything in the staging area (`tmp/`);
- deletes every file under `blobs/` that no row references, then prunes empty
  shard directories;
- leaves every referenced (committed) file untouched.

## Failure injection

The compose stack sets `UPLOAD_ENABLE_FAILPOINTS=1`. A request may then carry
`X-Upload-Failpoint: <name>`:

| Name | Simulated failure |
|---|---|
| `write` / `write-crash` | after bytes streamed, before rename |
| `rename`, `commit` (and `-crash`) | after filesystem publish, before DB commit |
| `respond` / `respond-crash` | after DB commit, before the response |

Plain names fail the request in-process (`500`). `*-crash` names call
`os.Exit(2)` immediately, killing the process in that exact gap; the
`restart: always` policy brings it back so real startup recovery runs. The
`verify` service asserts at each point that nothing readable-but-incomplete
exists, that leftovers are reaped after a restart, and that committed files
survive.

Leave `UPLOAD_ENABLE_FAILPOINTS` unset in production.

## Configuration

| Env var | Default | Meaning |
|---|---|---|
| `UPLOAD_DATA_DIR` | `/data` | root containing `tmp/`, `blobs/` and the database |
| `UPLOAD_ADDR` | `:8080` | listen address |
| `UPLOAD_DB` | `$UPLOAD_DATA_DIR/uploads.db` | SQLite database path |
| `UPLOAD_ENABLE_FAILPOINTS` | _(unset)_ | enable `X-Upload-Failpoint` (test only) |

## Local development

```sh
go test ./...                       # unit + in-process integration tests
go run ./cmd/uploadsrv              # serve on :8080 (set UPLOAD_DATA_DIR)
go run ./cmd/verify -mode healthcheck -base http://127.0.0.1:8080
```

The SQLite driver is the pure-Go `modernc.org/sqlite`, so the binaries build
with `CGO_ENABLED=0` and run on a static, libc-free container image.
