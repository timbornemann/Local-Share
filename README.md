# Local Share

Local Share is a tiny LAN web app for temporary file and text exchange. It has no accounts, no database, and no persistent storage by default. Shared items expire after 5 minutes unless a different lifetime is selected while sharing.

## Run Locally

```powershell
go run .
```

Open [http://localhost:8080](http://localhost:8080).

## Run With Docker

```powershell
docker build -t local-share .
docker run --rm -p 8080:8080 local-share
```

Open the host machine from another device on the same network with:

```text
http://<host-ip>:8080
```

## Release Package

When a GitHub Release is published, the release workflow builds a multi-architecture Docker image and publishes it to GitHub Container Registry as:

```text
ghcr.io/timbornemann/local-share:<release-tag>
```

The workflow also attaches downloadable packages named like `local-share-v1.0.0-docker.zip` and `local-share-v1.0.0-docker.tar.gz`. Each package contains:

- `run.ps1` for Windows
- `run.sh` for macOS/Linux
- `docker-compose.yml`
- a short package README

After downloading and extracting the package, start Local Share with:

```powershell
.\run.ps1
```

or:

```sh
./run.sh
```

## API

- `GET /` opens the browser UI.
- `GET /api/items` lists active shares, including `protected`, `previewable`, and `previewKind` metadata.
- `GET /api/items/{id}` returns metadata for one active share.
- `POST /api/items/files` accepts multipart uploads with repeated `files` fields. Optional form fields `ttlSeconds` and `password` apply to all files and must be sent before the first `files` field. A password must not consist only of whitespace.
- `POST /api/items/text` accepts JSON: `{ "text": "...", "name": "optional", "ttlSeconds": 300, "password": "optional" }`.
- `POST /api/items/{id}/unlock` accepts JSON `{ "password": "..." }` and returns `{ "token": "...", "expiresAt": "..." }` for protected shares.
- `GET /api/items/{id}/download` downloads a file or text item. Protected items require `?token=...` or `X-Share-Token`.
- `GET /api/items/{id}/raw` returns text shares and text/code files as `text/plain`. Protected items require `?token=...` or `X-Share-Token`.
- `GET /api/items/{id}/view` streams browser-previewable files inline. Protected items require `?token=...` or `X-Share-Token`.
- `DELETE /api/items/{id}` removes an item.
- `GET /api/config` returns the active upload limits (`maxFileSize`, `maxTotalSize`, `maxItems`, `maxTextSize`; `0` means unlimited).
- `GET /api/events` streams Server-Sent Events when the list changes.

## Notes

- The app is intentionally open to the local network.
- The default lifetime is 5 minutes. The UI allows 1 to 1440 minutes per share.
- Optional passwords hide preview/download/copy until unlocked in the browser. Password hashes and metadata stay in memory only; temporary files are not encrypted on disk. Passwords are stored as salted PBKDF2-SHA256 hashes, and repeated wrong passwords lock unlocking out (5 failures per client and item for 1 minute, 60 per item for 5 minutes; answered with `429` and `Retry-After`).
- Uploads are streamed to temporary files under `/tmp/local-share` in the container.
- Upload limits protect the host from being filled up. Defaults: 1024 MB per file, 2048 MB across all active shares, 200 active shares, 5 MB per text share. Change them with `LOCAL_SHARE_MAX_FILE_MB`, `LOCAL_SHARE_MAX_TOTAL_MB`, `LOCAL_SHARE_MAX_ITEMS` and `LOCAL_SHARE_MAX_TEXT_MB`; `0` disables a limit. Oversized uploads are rejected with `413`, a full store or too many shares with `507`.
- Restarting the container clears all shares.
