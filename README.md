# photo-sync

A small, self-hosted photo review app for photographers. You upload previews of a shoot's
raw files, then share a link so the model can mark photos accept/reject/stars. **Raw files
never leave your machine.** Only resized, metadata-stripped JPEG previews are uploaded.

> **Status:** complete review loop: upload, owner UI, expiring reviewer links, a reviewer
> page in English and Japanese with accept/reject, stars and comments, and the owner's
> view of all feedback. Runs locally (with optional DynamoDB/S3 emulators) or on AWS
> (Lambda + API Gateway, deployed with SAM).

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
│        │             │ ───────────────────▶ │ (memory/DynamoDB)  (disk/S3) │
│  .review-manifest    │  3. complete         │                              │
│     .json            │ ───────────────────▶ │                              │
└──────────────────────┘                      └──────────────────────────────┘
                                                        ▲
                                    browser ────────────┘  sessions, photo grid, preview
```
Step 2 goes straight to the blob storage (S3, or the API's own `/dev/blob/` in disk mode),
not through the API code.

1. The **uploader** hashes each raw file and registers it with the API. The API dedupes by
   hash within a session and, for new photos, returns short-lived signed upload URLs.
2. The uploader generates two JPEGs from the raw's embedded preview (a 400 px thumbnail and
   a 2048 px preview) and uploads them **directly** to storage. The image bytes never pass
   through the API.
3. The uploader calls `complete`; the API checks both files exist and marks the photo
   `ready`.
4. The **owner UI** lists sessions and shows each session's photos using short-lived signed
   image URLs.
5. The owner creates an expiring **review link** for the model, `https://…/r#<token>`, and
   sends it. The reviewer page lets them accept/reject, star and comment on each photo;
   everything saves automatically. The owner sees each reviewer's feedback and progress,
   can filter by verdict and stars, and can copy the matching filenames.

## Components

There are two programs plus the storage they use. Both programs are built from shared
packages under `internal/`.

### API server: `cmd/api`

An HTTP server that owns all state. It:

- serves the **JSON API** under `/api/`: owner routes (sessions, photo registration,
  upload completion, photo listing with feedback, reviewer links) authenticated with the
  owner API key (`Authorization: Bearer <key>`) or the owner's browser session cookie, and
  reviewer routes under `/api/review` authenticated with a reviewer session cookie;
- serves the **owner web UI** at `/`: sign in with the API key, browse sessions, manage
  review links, view each session's photos with everyone's feedback, filter by verdict and
  stars, open full-screen previews (← / → to navigate);
- serves the **reviewer page** at `/r`: opened from a review link; English or Japanese
  (auto-detected, switchable); accept/reject, 1–5 stars and a comment per photo, saved
  automatically;
- in disk mode, serves **blob storage** under `/dev/blob/`: signed, expiring upload and
  download URLs that mimic S3 presigned URLs. With S3 configured this route doesn't exist;
  URLs point at the bucket instead.

Locally it is a plain HTTP server. On AWS the same app runs as a Lambda function,
`cmd/lambda`, behind an API Gateway HTTP API (see [Deploying to AWS](#deploying-to-aws)).

### Uploader CLI: `cmd/uploader`

Runs on the photographer's machine against one folder of raw files (Sony `.ARW`, Nikon
`.NEF`):

- includes raws in subfolders (e.g. `100MSDCF`, `101MSDCF` after the camera rolls over at
  file 9999). Each photo is named by its path within the folder, so photos stay in shot
  order and the same filename in two folders stays distinct; hidden files and folders are
  skipped;
- creates a session on first run and records it in `.review-manifest.json` inside the
  folder, so re-runs reuse the session and skip photos already uploaded;
- processes files in parallel (`-workers`), extracting previews in pure Go: no exiftool,
  libvips or other installs;
- resumes interrupted uploads on the next run; `-force` re-uploads previews for existing
  photos (e.g. after changing preview settings).

### Storage

| Data | Local / default | AWS |
|---|---|---|
| Sessions, photos, reviewers, feedback | In memory (lost on restart) or DynamoDB Local | DynamoDB (single table, on-demand) |
| Preview images | Local disk (`./data`) or a local S3-compatible server | S3, private bucket |

### Internal packages

| Package | Purpose |
|---|---|
| `internal/app` | Composition root: builds the whole HTTP app (API, UI, local blob route) from a store and blob store |
| `internal/api` | Wire contract shared by server and clients: routes, request/response types, validation |
| `internal/server` | HTTP layer: routing, auth, JSON, mapping errors to status codes |
| `internal/review` | Use cases and business rules (dedupe, upload confirmation, reviewer links, feedback); no HTTP |
| `internal/store` | Persistence interface with `Memory` and `Dynamo` implementations |
| `internal/blob` | Object storage interface (presigned PUT/GET) with `Local` (disk) and `S3` implementations |
| `internal/model` | Domain types (`Session`, `Photo`, `Reviewer`, `Feedback`) and ID generation (ULIDs) |
| `internal/ui` | Owner UI (`/`) and reviewer page (`/r`): static HTML/CSS/JS embedded into the binary |
| `internal/lambdahttp` | Runs an `http.Handler` on Lambda: translates API Gateway events to HTTP requests and back |
| `internal/uploader` | The uploader's pipeline: scan, hash, preview, upload, manifest |
| `internal/apiclient` | Go client for the API, used by the uploader |
| `internal/preview` | Raw file → thumbnail + preview JPEGs (orientation-corrected, EXIF stripped) |
| `internal/manifest` | Reads and writes `.review-manifest.json` atomically |
| `internal/testserver` | Starts the full local API stack for client-side tests |

Layering in the API: `server` → `review` → `store` / `blob`. Swapping memory for DynamoDB
(or disk for S3) doesn't touch the HTTP or business-rule code. Dependencies are wired by
hand (no DI container): `cmd/api` picks the implementations from the environment and
passes them to `app.New`, which tests use too.

## Running locally

**Prerequisites:** Go 1.27+. Docker is optional (only for the local DynamoDB/S3
emulators).

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

Go to http://localhost:8080 and sign in with the API key. The browser exchanges it for a
7-day session cookie that page scripts can't read; the key itself isn't stored. Use a
current Chrome, Edge or Firefox: the cookie is `Secure`, which those browsers allow on
`http://localhost`.

Unless `PHOTOSYNC_SESSION_SECRET` is set, restarting the API signs you out.

### 4. Send a review link

Open a session, enter the reviewer's name under **Reviewers**, choose how long the link
stays valid and click **Create link**. Copy the link straight away: it's shown once (only
a hash of it is stored). **Extend** pushes the expiry out; **Revoke** disables it
immediately and permanently (their feedback is kept).

The reviewer opens the link, which signs them in with a cookie of their own; reopening
the page later works while the link is valid. To try it locally, open the link in a
private window so the owner and reviewer cookies don't mix.

Back on the session page you'll see each reviewer's verdicts, stars and comments, their
progress, and a filter by verdict and minimum stars. **Copy filenames** copies the
filtered filenames one per line, e.g. to select keepers in Lightroom or Capture One. It
copies names without their folder; if a copied name also exists in another folder (after a
counter rollover), the button says so, since pasting it would match both.

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

### Running against S3 locally (RustFS)

To exercise the S3 code path without AWS, run [RustFS](https://github.com/rustfs/rustfs),
an S3-compatible server. (MinIO's images now require a registry login.)

```powershell
docker run --rm -p 9000:9000 -e RUSTFS_ACCESS_KEY=devkey -e RUSTFS_SECRET_KEY=devsecret123 rustfs/rustfs

$env:PHOTOSYNC_API_KEY="devkey"
$env:PHOTOSYNC_S3_BUCKET="photosync-previews"
$env:PHOTOSYNC_S3_ENDPOINT="http://localhost:9000"
$env:AWS_ACCESS_KEY_ID="devkey"
$env:AWS_SECRET_ACCESS_KEY="devsecret123"
go run ./cmd/api
```

The API creates the bucket on first start. Combine with the DynamoDB variables above to
run the same storage stack as on AWS.

## Deploying to AWS

The stack is defined in [`deploy/template.yaml`](deploy/template.yaml) (AWS SAM) and
deployed from your machine with [`deploy/deploy.ps1`](deploy/deploy.ps1). No CI pipeline
is needed.

```
browser / uploader ──HTTPS──▶ API Gateway (HTTP API) ──▶ Lambda (cmd/lambda) ──▶ DynamoDB
        │                                                     │
        └──── presigned URLs (images, uploads) ──▶ S3 bucket ◀─┘ (signs URLs)
                                                              └──▶ SSM Parameter Store
                                                                   (API key hash, session secret)
```

| Resource | Notes |
|---|---|
| API Gateway HTTP API | Single catch-all route to the function; throttled to 20 req/s (burst 50); access logs (paths only) |
| Lambda | `provided.al2023`, arm64, 256 MB; least-privilege role for exactly the table, bucket and two parameters |
| DynamoDB table | On-demand; same layout as `store.CreateTable` (incl. LSI1) |
| S3 bucket | Private (all public access blocked, HTTPS only), previews expire after 90 days |
| SSM parameters | `/photosync/api-key-hash`, `/photosync/session-secret` (SecureString), created by the script |

The table and bucket are kept (`DeletionPolicy: Retain`) if the stack is deleted, so data
survives a `sam delete`; remove them by hand if you really want them gone.

### One-time setup

1. Install the [AWS CLI v2](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html)
   and [AWS SAM CLI](https://docs.aws.amazon.com/serverless-application-model/latest/developerguide/install-sam-cli.html)
   (or `winget install Amazon.AWSCLI` / `winget install Amazon.SAM-CLI`), then open a new
   terminal.
2. Configure credentials for your account
   ([guide](https://docs.aws.amazon.com/cli/latest/userguide/cli-chap-configure.html)):
   `aws configure sso` (IAM Identity Center, recommended) or `aws configure` (IAM user
   access keys). Don't use the root user. Pick the region to deploy to.
3. Check: `aws sts get-caller-identity` shows the intended account.

### Deploy

```powershell
.\deploy\deploy.ps1
```

If Windows refuses with "running scripts is disabled on this system", allow local scripts
once for your user with `Set-ExecutionPolicy -Scope CurrentUser RemoteSigned`, or run
`powershell -ExecutionPolicy Bypass -File .\deploy\deploy.ps1` for a single run.

The script prints the target account and region, creates the two secrets if missing
(**printing a newly generated API key once**; store it in a password manager), builds the
Lambda binary, and runs `sam deploy`. The first run is guided (accept the defaults; answer
**y** to the IAM role question) and saves your answers to `deploy/samconfig.toml`. Every
run shows the CloudFormation change set and asks before applying it. At the end it prints
the app URL.

Then:

```powershell
$env:PHOTOSYNC_API_URL = "https://<id>.execute-api.<region>.amazonaws.com"
$env:PHOTOSYNC_API_KEY = "<the key printed by the script>"
go run ./cmd/uploader "D:\Shoots\2026-11-09 Ana"
```

and open the URL in a browser to sign in.

Options: `-NewApiKey` rotates the owner API key, `-AwsProfile <name>` uses a named CLI
profile, `-Region <region>` overrides the region, `-Guided` re-runs the SAM questions.

To remove the app: `sam delete --stack-name photosync` (from `deploy/`). The table, bucket
and SSM parameters stay until you delete them.

## Configuration

### API (`cmd/api`)

| Variable | Default | Purpose |
|---|---|---|
| `PHOTOSYNC_API_KEY` | *(required)* | Owner API key for the CLI and web UI; the server keeps only its SHA-256 hash |
| `PHOTOSYNC_SESSION_SECRET` | *(random per process)* | Signs browser session cookies; at least 32 bytes |
| `PHOTOSYNC_ADDR` | `localhost:8080` | Listen address |
| `PHOTOSYNC_BASE_URL` | `http://<addr>` | Externally reachable origin, used in signed image/upload URLs |
| `PHOTOSYNC_DATA_DIR` | `data` | Where preview images are stored in disk mode |
| `PHOTOSYNC_DYNAMODB_TABLE` | *(unset: memory)* | Use this DynamoDB table |
| `PHOTOSYNC_DYNAMODB_ENDPOINT` | *(unset: AWS)* | DynamoDB Local endpoint; creates the table if missing and uses dummy credentials |
| `PHOTOSYNC_S3_BUCKET` | *(unset: disk)* | Store preview images in this S3 bucket |
| `PHOTOSYNC_S3_ENDPOINT` | *(unset: AWS)* | S3-compatible server; creates the bucket if missing. Credentials from `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` |

Without an endpoint override, the API uses real AWS through the standard credential chain
(`AWS_PROFILE`, `AWS_REGION`, ...), and the table and bucket must already exist.

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

Runs everything that needs no external services. The store and blob packages each have
one shared test suite that every implementation must pass. To include the AWS-backed
implementations, run the local emulators and point the tests at them:

```powershell
docker run --rm -d -p 8000:8000 amazon/dynamodb-local
docker run --rm -d -p 9000:9000 -e RUSTFS_ACCESS_KEY=testkey -e RUSTFS_SECRET_KEY=testsecret123 rustfs/rustfs

$env:PHOTOSYNC_DYNAMODB_TEST_ENDPOINT="http://localhost:8000"
$env:PHOTOSYNC_S3_TEST_ENDPOINT="http://localhost:9000"
$env:PHOTOSYNC_S3_TEST_ACCESS_KEY="testkey"
$env:PHOTOSYNC_S3_TEST_SECRET_KEY="testsecret123"
go test ./internal/store ./internal/blob
```

Each test creates and deletes its own table or bucket. The tests refuse a real AWS
endpoint.

The preview pipeline can also be checked against your own raw files:

```powershell
$env:PREVIEW_SAMPLE_DIR="D:\Shoots\samples"
go test ./internal/preview -run TestRealRaws -v
```

## Security notes

- Raw files are never uploaded; previews are re-encoded, which strips all EXIF (including
  GPS).
- Upload and image URLs are signed per object and HTTP method and expire (15 min for
  uploads, 1 h for viewing). S3 upload URLs also sign `Content-Type: image/jpeg`. Request
  logs record paths only, never the signatures in query strings.
- The S3 bucket stays private; images are only reachable through those signed URLs. S3
  upload URLs can't cap object size (disk mode caps at 25 MB), so a leaked upload URL could
  store a large object until it expires.
- The owner UI is served with a strict Content Security Policy, `no-referrer` and
  `noindex`, and never inserts user-supplied text as HTML.
- Owner authentication: the uploader sends the API key as `Authorization: Bearer`; the
  browser trades it once (`POST /api/login`) for a signed, expiring session cookie
  (`HttpOnly`, `Secure`, `SameSite=Strict`). The server stores only the key's SHA-256
  hash and compares in constant time. Use a long random key, e.g. in PowerShell:
  `$b = New-Object byte[] 32; [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($b); [Convert]::ToBase64String($b)`.
- Session cookies are stateless: logging out deletes the cookie, but a copy taken earlier
  stays valid until it expires (7 days). Changing the session secret signs every browser
  out.
- Cross-site requests that would change state (e.g. a form on another site posting to
  the API) are rejected using Go's `http.CrossOriginProtection`.
- Review links carry a 256-bit random token in the URL **fragment** (`/r#<token>`), which
  browsers never send to the server, so it can't appear in access logs or `Referer`
  headers. The page exchanges it for a reviewer cookie on each load and leaves it in the
  address bar, so the link keeps working when an app's in-app browser (Instagram, LINE)
  hands the page to the phone's real browser, which has separate cookies. The same link
  is already in the reviewer's chat app, so this exposes nothing new; revoking or
  expiring the link still ends access immediately. Only the token's SHA-256 is stored.
- Every reviewer request re-checks the link, so expiry and revocation apply immediately.
  A reviewer sees only their session's ready photos and their own feedback, never
  filenames, hashes or other reviewers' feedback. The reviewer page has no link-preview
  metadata, so messaging apps don't show photos when the link is shared.

## Roadmap

1. Writing ratings back to raw metadata (XMP) via the manifest
2. Export as CSV (UTF-8 with BOM for Excel), deleting sessions and photos
3. Per-folder "shoot mode" in the uploader (one session per model subfolder)
