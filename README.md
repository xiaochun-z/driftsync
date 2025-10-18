# DriftSync

DriftSync is a one-shot, bidirectional synchronization tool between a local folder and Microsoft OneDrive.
It is written in Go and designed for speed, correctness, and safety — every run performs a full sync cycle and then exits cleanly.

## Key Features

* Bidirectional Sync — keeps local and cloud in sync (adds / modifies / deletes propagate both ways).

* Selective Sync (sync_list) — restrict synchronization to specific paths or patterns.

* Conflict Safe — if both local and cloud change the same file, both versions are kept:

1. file.local-conflict-YYYYmmdd-HHMMSS.ext

2. file.cloud-conflict-YYYYmmdd-HHMMSS.ext

3. Large File Support — files > 4 MB are uploaded through createUploadSession with chunked upload, automatic retry, and resume.

4. Fast and Reliable — parallel upload/download workers, SHA1-based skip of unchanged files, and connection pooling.

One-Shot Mode — DriftSync runs one full cycle (delete → upload → download) and then exits.

SQLite State DB — stores tokens, ETags, hashes, and timestamps to detect incremental changes.

## Configuration

Create config.yaml (or copy from config.sample.yaml):
```
tenant: consumers                 # or 'common' / your AAD tenant
client_id: "<YOUR_AAD_APP_CLIENT_ID>"
local_path: "/absolute/path/to/sync"
download_from_cloud: true
upload_from_local: true
sync_list_path: "/absolute/path/to/sync_list"
download_workers: 8
upload_workers: 8
upload_chunk_mb: 8
upload_parallel: 2
```
## sync_list Syntax

The sync_list file lets you precisely control which files or folders are synchronized.
Its syntax is line-based and pattern-driven, as implemented in internal/selective.

Each non-empty line is interpreted as follows:
| Form                     | Meaning                                                    |
| ------------------------ | ---------------------------------------------------------- |
| `path/or/prefix`         | include all files under this relative path (prefix match). |
| `-pattern`               | exclude files or directories matching this pattern.        |
| blank line / `# comment` | ignored.                                                   |

### Behavior Rules

* Inclusion lines start without - and act as path prefixes.
Example:
```
docs
images/
```
includes all files whose relative paths start with docs or images/.

* Exclusion lines start with - and may contain * or ? wildcards.
Example:
```
-*.tmp
-backup/*
```
excludes any file ending with .tmp or any file under backup/.

* Paths are relative to local_path, always using forward slashes /.
(Example : subdir/file.txt, not C:\\path\\...)

* Matching is case-sensitive on Linux and case-insensitive on Windows, following Go’s default filepath.Match behavior.

If sync_list_path is empty or missing, all files are included (no filters).
```
# include everything under /docs
/docs

# include images but exclude temporary files
/images
-*.tmp
```

This will synchronize:

1. docs/readme.md ✅

2. images/photo.jpg ✅

3. notes/todo.md ❌ (not under included prefix)

4. images/tmp123.tmp ❌ (matches -*.tmp)

## Usage
```
go mod tidy
go build ./cmd/driftsync
./driftsync
```