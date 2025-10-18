# DriftSync v0.5

A fast, one-shot, bi-directional OneDrive sync tool written in Go.

## Key Features
- One-shot sync: run once and exit.
- Selective sync via `sync_list` file (abiding common abraunegg patterns).
- Bi-directional sync (adds/mods/deletes both sides).
- Safe conflict handling: when both sides changed, keep **both** versions with timestamped suffixes.
- Skip unchanged files (SHA1) + short-term bounce suppression.
- High throughput: parallel workers + tuned HTTP pool.
- Large files: **resumable chunked upload** via `createUploadSession`.
- English-only logs and comments. Clean build (no unused imports).

## Build
```bash
go mod tidy
go build ./cmd/driftsync
```

## Config
Copy `config.sample.yaml` to `config.yaml` and edit:

```yaml
tenant: consumers
client_id: "<YOUR_APP_CLIENT_ID>"
local_path: "/path/to/folder"
download_from_cloud: true
upload_from_local: true
sync_list_path: ""          # empty => sync everything
download_workers: 8
upload_workers: 8
upload_chunk_mb: 8          # chunk size for large file upload
upload_parallel: 2          # parallel chunk uploads per file (1-4 recommended)
```
