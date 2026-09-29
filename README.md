# Upload Service

A small, strictly-validated file upload service: **Go + SQLite + a local
content-addressed file directory**.

Each request is a `multipart/form-data` body containing **exactly**:

1. one JSON metadata field named `meta`;
2. one file field named `file`, at most **16 MiB**.

Files are hashed with SHA-256 **while streaming to disk**, staged in a
temporary directory, and only become readable after their database row is
committed. The client-supplied filename is never used as a path.

---

## Quick start (fixed acceptance)

The repository ships a runnable Compose stack and a one-shot `verify`
acceptance service. Run them in order:

```bash
docker compose config --quiet
docker compose build
docker compose run --rm verify
```

`verify` exits `0` only when every black-box and crash/recovery check passes.
To try the API manually:

```bash
docker compose up -d server
curl -s localhost:8080/healthz

curl -s -X POST localhost:8080/uploads \
  -F 'meta={"request_id":"demo-1","content_type":"text/plain"};type=application/json' \
  -F 'file=@./some-file.txt;type=text/plain'

curl -s localhost:8080/uploads/demo-1            # metadata record
curl -s localhost:8080/uploads/demo-1/content    # bytes back
```

---

## HTTP API

| Method & path                        | Meaning                                             |
|--------------------------------------|-----------------------------------------------------|
| `POST /uploads`                      | Upload metadata + file (multipart/form-data)        |
| `GET  /uploads/{request_id}`         | Fetch the JSON record                               |
| `GET  /uploads/{request_id}/content` | Fetch the stored bytes (range/conditional supported)|
| `GET  /healthz`                      | Liveness probe                                      |

### Request

```
POST /uploads
Content-Type: multipart/form-data; boundary=...

--...
Content-Disposition: form-data; name="meta"
Content-Type: application/json

{"request_id":"abc-123","content_type":"image/png"}
--...
Content-Disposition: form-data; name="file"; filename="anything.png"
Content-Type: image/png

<... up to 16 MiB ...>
--...--
```

* `request_id` (required, <= 200 chars, no `/ ? # %`) is the idempotency key
  and the only identifier the service stores.
* `content_type` (optional) must be a bare media type with **no parameters**.
* The `filename` parameter is accepted for compatibility but is ignored -- the
  blob is stored under its SHA-256, so path traversal in filenames cannot
  influence the filesystem layout.

### Responses

* `201 Created` on first commit; `200 OK` on an idempotent replay.
* `409 Conflict` if the same `request_id` is reused with different content.
* `4xx` for every malformed request -- **the entire request fails** and no
  partial state is kept:
  * missing / duplicate / unknown multipart fields,
  * unknown / duplicate JSON keys, invalid JSON, trailing JSON data,
  * truncated body (fewer bytes than the declared `Content-Length`),
  * file larger than 16 MiB, empty file, wrong content type,
  * missing `Content-Length` (chunked uploads are rejected).

Record response:

```json
{
  "request_id": "abc-123",
  "sha256": "9f86d081...",
  "size": 1234,
  "content_type": "image/png",
  "created_at": "2026-01-01T00:00:00Z"
}
```

### Idempotency and conflicts

Uploading the same `request_id` **with byte-identical content** (same SHA-256
and size) returns the original record with `200`, even if the metadata
`content_type` or the filename differs; the first commit always wins. The
replayed body is still fully streamed and hashed, so a truncated replay cannot
be mistaken for a success.

The same `request_id` with **different content** returns `409 Conflict` and
creates nothing.

---

## Storage model & crash safety

```
/data
+-- uploads.db            # SQLite: one row per committed upload
+-- uploads.db-wal
+-- tmp/                  # staging area (upload-<ts>-<rand>.tmp)
+-- blobs/<aa>/<62 hex>   # content-addressed blobs: first 2 hash chars shard
```

The commit pipeline is:

1. **write** -- bytes stream through a 32 KiB buffer into a freshly created
   temp file in `tmp/` and through a SHA-256 hasher at the same time. The full
   file is never held in memory, and the temp name contains no client input.
2. validate framing (exactly two parts, body complete, limits respected).
3. **rename** -- `fsync` the file, atomically `rename(2)` it to
   `blobs/<shard>/<hash>` on the same filesystem, `fsync` the directory.
4. **commit** -- insert the SQLite row (`synchronous=FULL`, WAL).
5. respond.

An upload becomes readable only once its database row exists; reads always
locate the blob through the hash stored in the row.

### Recovery on restart

Before listening, the reconciler:

* deletes every leftover file in `tmp/` (crashed during/after the write);
* deletes any blob under `blobs/` that has **no referencing row** (crashed
  after rename, before commit);
* never touches blobs that have a row -- committed uploads always survive.

This is exercised end to end by `verify`, which injects hard process crashes
at each stage.

## Failure injection (`verify` only)

The server honours an `X-Upload-Fault: after_write | after_rename |
after_commit` header **only when started with `-faults`**. The production
compose service runs without it, so the header is ignored there. With it, the
process calls `os.Exit(3)` at that exact point -- including *after the
database commit but before the response* -- proving clients can safely retry
and that no half-finished upload is ever readable.

`docker compose run --rm verify` covers:

* the full strict-validation matrix against the production server;
* the exact 16 MiB boundary (16 MiB accepted, 16 MiB+1 -> 413);
* a truncated connection (fewer bytes sent than `Content-Length`);
* idempotent replay and conflict;
* content retrieval, ETag, and the fact that the client filename never leaks;
* crashes after write / rename / commit, with restart and on-disk inspection;
* that previously committed files are never removed by recovery.

## Local development

```bash
go test ./...

go build -o /tmp/server ./cmd/server
go build -o /tmp/verify ./cmd/verify
/tmp/server -addr :8080 -data ./data &
BASE_URL=http://127.0.0.1:8080 SERVER_BIN=/tmp/server \
  FAULT_DATA=/tmp/faultdata FAULT_ADDR=127.0.0.1:18080 /tmp/verify
```

The SQLite driver is the pure-Go `modernc.org/sqlite`, so the image builds
with `CGO_ENABLED=0`.
