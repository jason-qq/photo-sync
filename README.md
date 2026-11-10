# photo-sync

A small, self-hosted photo review app for photographers. You upload previews of a shoot's
raw files, then share a link so the model can mark photos accept/reject/stars. **Raw files
never leave your machine.** Only resized, metadata-stripped JPEG previews are uploaded.

> **Status:** works end to end locally: uploader, API, owner web UI, and a DynamoDB store
> (tested against DynamoDB Local). Not yet built: S3 storage, AWS deployment, reviewer
> links and feedback.

## How it works

```
 your machine                                   API server (cmd/api)
┌──────────────────────┐                      ┌──────────────────────────────┐
│ folder of .ARW/.NEF  │                      │ /api/...    JSON API         │
│        │             │  1. register photo   │ /           owner web UI     │
│  uploader CLI        │ ───────────────────▶ │                              │
│  • hash raw          │ ◀── upload URLs ──── │ review service               │
│  • make previews     │                      │   │                 │        │
│  • strip EXIF        │  2. PUT JPEGs        │ store               blob     │
│        │             │ ───────────────────▶ │ (memory/DynamoDB)  (disk*)   │
│  .review-manifest    │  3. complete         │                              │
│     .json            │ ───────────────────▶ │                              │
└──────────────────────┘                      └──────────────────────────────┘
                                                        ▲
                                    browser ────────────┘  sessions, photo grid, preview
```
\* Local disk stands in for S3 until the S3 store is built.

1. The **uploader** hashes each raw file and registers it with the API. The API dedupes by
   hash within a session and, for new photos, returns short-lived signed upload URLs.
2. The uploader generates two JPEGs from the raw's embedded preview (a 400 px thumbnail and
   a 2048 px preview) and uploads them **directly** to storage. The image bytes never pass
   through the API.
3. The uploader calls `complete`; the API checks both files exist and marks the photo
   `ready`.
4. The **owner UI** lists sessions and shows each session's photos using short-lived signed
   image URLs.

## Components

There are two programs plus the storage they use. Both programs are built from shared
packages under `internal/`.

### API server: `cmd/api`

An HTTP server that owns all state. It:

- serves the **JSON API** under `/api/` (sessions, photo registration, upload completion,
  photo listing), authenticated with the owner API key (`Authorization: Bearer <key>`);
- serves the **owner web UI** at `/`: sign in with the API key, browse sessions, view a
  session's thumbnail grid, open full-screen previews (← / → to navigate);
- in local mode, serves **blob storage** under `/dev/blob/`: signed, expiring upload and
  download URLs that mimic S3 presigned URLs.

It is designed to run on AWS Lambda later. Locally it is a plain HTTP server.

### Uploader CLI: `cmd/uploader`

Runs on the photographer's machine against one folder of raw files (Sony `.ARW`, Nikon
`.NEF`):

- creates a session on first run and records it in `.review-manifest.json` inside the
  folder, so re-runs reuse the session and skip photos already uploaded;
- processes files in parallel (`-workers`), extracting previews in pure Go: no exiftool,
  libvips or other installs;
- resumes interrupted uploads on the next run; `-force` re-uploads previews for existing
  photos (e.g. after changing preview settings).

### Storage

| Data | Local / default | AWS |
|---|---|---|
| Sessions, photos | In memory (lost on restart) or DynamoDB Local | DynamoDB (single table, on-demand) |
| Preview images | Local disk (`./data`) | S3, private bucket *(not built yet)* |

### Internal packages

| Package | Purpose |
|---|---|
| `internal/api` | Wire contract shared by server and clients: routes, request/response types, validation |
| `internal/server` | HTTP layer: routing, auth, JSON, mapping errors to status codes |
| `internal/review` | Use cases and business rules (dedupe, upload confirmation, listing); no HTTP |
| `internal/store` | Persistence interface with `Memory` and `Dynamo` implementations |
| `internal/blob` | Object storage interface (presigned PUT/GET) with a disk-backed `Local` implementation |
| `internal/model` | Domain types (`Session`, `Photo`) and ID generation (ULIDs) |
| `internal/ui` | Owner web UI: static HTML/CSS/JS embedded into the binary |
| `internal/uploader` | The uploader's pipeline: scan, hash, preview, upload, manifest |
| `internal/apiclient` | Go client for the API, used by the uploader |
| `internal/preview` | Raw file → thumbnail + preview JPEGs (orientation-corrected, EXIF stripped) |
| `internal/manifest` | Reads and writes `.review-manifest.json` atomically |
| `internal/testserver` | Starts the full local API stack for client-side tests |

Layering in the API: `server` → `review` → `store` / `blob`. Swapping memory for DynamoDB
(or disk for S3) doesn't touch the HTTP or business-rule code.

## Running locally

**Prerequisites:** Go 1.27+. Docker is optional (only for DynamoDB Local).

Examples use PowerShell; in bash, write `VAR=value command` instead of `$env:VAR="value";`.

### 1. Start the API

```powershell
$env:PHOTOSYNC_API_KEY="devkey"
go run ./cmd/api
```

Listens on http://localhost:8080 and stores preview images under `./data`. Pick any API
key; the uploader and the browser must use the same one.

### 2. Upload a folder of raws

In a second terminal:

```powershell
$env:PHOTOSYNC_API_KEY="devkey"
go run ./cmd/uploader "D:\Shoots\2026-11-09 Ana"
```

Useful flags: `-title "Ana – studio"`, `-date 2026-11-09` (both only for a new session),
`-workers 8`, `-force`. Re-running on the same folder only uploads what's new or
unfinished. Ctrl+C is safe; run again to continue.

### 3. Open the owner UI

Go to http://localhost:8080 and sign in with the API key. The key is kept in the browser
tab's session storage and cleared when the tab closes.

### Keeping data across restarts (DynamoDB Local)

By default sessions live in memory, so restarting the API loses them (images in `./data`
remain, but nothing references them). To persist, run DynamoDB Local:

```powershell
docker run --rm -p 8000:8000 amazon/dynamodb-local

$env:PHOTOSYNC_API_KEY="devkey"
$env:PHOTOSYNC_DYNAMODB_TABLE="photosync"
$env:PHOTOSYNC_DYNAMODB_ENDPOINT="http://localhost:8000"
go run ./cmd/api
```

The API creates the table on first start. As written, DynamoDB Local itself keeps data in
memory: stopping the container wipes it. To keep it across container restarts, store it
in a Docker volume:

```powershell
docker run --rm -p 8000:8000 --user root -v photosync-ddb:/home/dynamodblocal/data amazon/dynamodb-local -jar DynamoDBLocal.jar -sharedDb -dbPath ./data
```

(`--user root` is needed because a new volume is owned by root, while the image runs as an
unprivileged user that couldn't create its database file there.)

The uploader's manifest remembers the session ID, so if the API's data is wiped, delete
`.review-manifest.json` from the folder to start a new session.

## Configuration

### API (`cmd/api`)

| Variable | Default | Purpose |
|---|---|---|
| `PHOTOSYNC_API_KEY` | *(required)* | Owner API key for the CLI and web UI |
| `PHOTOSYNC_ADDR` | `localhost:8080` | Listen address |
| `PHOTOSYNC_BASE_URL` | `http://<addr>` | Externally reachable origin, used in signed image/upload URLs |
| `PHOTOSYNC_DATA_DIR` | `data` | Where preview images are stored |
| `PHOTOSYNC_DYNAMODB_TABLE` | *(unset: memory)* | Use this DynamoDB table |
| `PHOTOSYNC_DYNAMODB_ENDPOINT` | *(unset: AWS)* | DynamoDB Local endpoint; creates the table if missing and uses dummy credentials |

With a table but no endpoint, the API uses real AWS through the standard credential chain
(`AWS_PROFILE`, `AWS_REGION`, ...). The table must then already exist.

### Uploader (`cmd/uploader`)

| Variable | Default | Purpose |
|---|---|---|
| `PHOTOSYNC_API_KEY` | *(required)* | Owner API key |
| `PHOTOSYNC_API_URL` | `http://localhost:8080` | API base URL |

The manifest records the API URL it was created against; running the same folder against
a different API is refused, so a local test folder can't be pushed to production by
accident.

## Testing

```powershell
go test ./...
```

Runs everything that needs no external services. The store tests are one shared suite that
both stores must pass; to include DynamoDB, run DynamoDB Local and point the tests at it:

```powershell
docker run --rm -d -p 8000:8000 amazon/dynamodb-local
$env:PHOTOSYNC_DYNAMODB_TEST_ENDPOINT="http://localhost:8000"
go test ./internal/store
```

Each test creates and deletes its own table. The tests refuse a real AWS endpoint.

The preview pipeline can also be checked against your own raw files:

```powershell
$env:PREVIEW_SAMPLE_DIR="D:\Shoots\samples"
go test ./internal/preview -run TestRealRaws -v
```

## Security notes

- Raw files are never uploaded; previews are re-encoded, which strips all EXIF (including
  GPS).
- Upload and image URLs are signed per object and HTTP method and expire (15 min for
  uploads, 1 h for viewing). Request logs record paths only, never the signatures in query
  strings.
- The owner UI is served with a strict Content Security Policy, `no-referrer` and
  `noindex`, and never inserts user-supplied text as HTML.
- The API key in the web UI is a local-development measure; a proper owner login is planned
  for deployment.

## Roadmap

1. S3 blob store
2. AWS deployment (Lambda + API Gateway, DynamoDB, S3)
3. Reviewer links (expiring capability URLs), reviewer UI, accept/reject/stars/comments
4. Export of keepers (filename list) and, later, writing ratings back to raw metadata
