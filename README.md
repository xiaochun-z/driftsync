# DriftSync

**DriftSync** is a one-shot, bidirectional synchronization tool between a local folder and Microsoft OneDrive.  
Written in Go, it focuses on speed, correctness, and safety — each run performs a full synchronization cycle and then exits cleanly.

# Disclaimer

DriftSync performs automatic file synchronization between your local system and Microsoft OneDrive.
While every effort has been made to ensure safe and predictable operation, data loss is always a risk when performing bidirectional sync operations.

If any file is accidentally deleted or modified, you may be able to restore it from the OneDrive web interface at https://onedrive.live.com under Recycle Bin.
However, recovery cannot be guaranteed — OneDrive retention policies and account settings may limit the availability of deleted items.

You are strongly advised to maintain your own independent backups of all important data before using DriftSync.
By using this software, you acknowledge and agree that DriftSync, its contributors, and maintainers assume no responsibility or liability for any data loss, corruption, or other damages arising from its use.
If you do not agree with these terms, do not use this program.

---

## Key Features

- **Bidirectional Sync** — Keeps local and cloud fully aligned.  
  Additions, modifications, and deletions propagate in both directions.  
  DriftSync always follows the newest state: if a file was deleted on the cloud, it will be removed locally, and vice versa.

- **Selective Sync (`sync_list`)** — Synchronize only specific paths or patterns.

- **Conflict-Safe** — If both sides changed the same file, DriftSync preserves both:  
  1. `file.local-conflict-YYYYmmdd-HHMMSS.ext`  
  2. `file.cloud-conflict-YYYYmmdd-HHMMSS.ext`

- **Large File Support** — Files > 4 MB are uploaded using `createUploadSession` with chunked transfer and automatic retry (without resume after exit).

- **Fast and Reliable** — Parallel upload/download workers, SHA-1 based skipping of unchanged files, and efficient connection reuse.

- **One-Shot Mode** — Runs one full sync cycle (delete → upload → download) and then exits.

- **SQLite State DB** — Persists tokens, ETags, hashes, and timestamps for accurate incremental detection.

---

## Configuration

Create `config.yaml` (or copy from `config.sample.yaml`):

```yaml
tenant: consumers                 # or 'common' / your AAD tenant ID
client_id: "3ae224c9-f16c-42d1-bf5d-44151f2b99fa"
local_path: "/absolute/path/to/sync"
download_from_cloud: true
upload_from_local: true
sync_list_path: "/absolute/path/to/sync_list"   # empty = sync everything
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