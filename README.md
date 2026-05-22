# driftsync

`driftsync` is a command-line tool for fast, reliable, incremental synchronization between a **local directory** and **Microsoft OneDrive** using the **Delta API**.
It is optimized for large folder structures, low-change environments, and long‑running use inside Dev Containers, Linux servers, WSL, or Windows hosts.

---

## ✨ Core Features

### 🔄 Incremental Sync via Delta API
- Tracks file changes using OneDrive Delta API
- Minimizes bandwidth usage
- Avoids re-uploading unchanged files

### 🗄 Local SQLite Metadata Store
- Keeps a persistent local database (`driftsync.db`) next to your config file
- Stores SHA-256 hashes, timestamps, and delta tokens
- Ensures safe resume and consistent sync across runs

### 🔐 Secure Authentication (Device Code Flow)
- Native Microsoft identity login (no browser dependency required)
- Tokens are stored locally and refreshed automatically

### 🚀 Lightweight & Cross‑Platform
- Written in Go, single binary, no runtime dependencies
- Runs on Linux, macOS, Windows, WSL, and Dev Containers

### 📁 Selective Sync Support
- Include or exclude specific paths via YAML config or a text sync-list file
- Exclude rules take priority over include rules

### 🛡 Safety Features
- Modified local files are never silently overwritten by cloud changes
- Deleted files are moved to `.driftsync_trash/` before removal (not permanently deleted)
- System files (`.DS_Store`, `Thumbs.db`, `desktop.ini`) are automatically ignored
- Conflict files are written alongside the original (e.g. `file.cloud-conflict-20240101-120000.md`)

---

## 📦 Installation

**Build from source:**

```bash
# Linux / macOS
go build -o driftsync ./cmd/driftsync

# Windows
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o driftsync.exe ./cmd/driftsync

# Embed version string at build time (optional)
go build -ldflags="-s -w -X main.version=v1.0.0" -trimpath -o driftsync ./cmd/driftsync
```

Requires Go 1.25 or later.

---

## ⚙️ Configuration

Copy `config.sample.yaml` and edit it:

```yaml
tenant: consumers           # "consumers" for personal accounts, "common" for any, or your Azure tenant GUID
client_id: "<your-client-id>"

local_path: "/var/data/onedrive"   # absolute or relative path to local sync root

download_from_cloud: true
upload_from_local: true

download_workers: 8
upload_workers: 8
upload_chunk_mb: 8          # chunk size for large file uploads (>4 MB)
upload_parallel: 2          # parallel chunk uploads per file (max 4)

interactive: false          # prompt to resolve conflicts instead of defaulting to "keep both"

log:
  list_checked: false       # log every file checked during upload scan
  verbose: false            # log every upload/download/delete action
```

### Config field reference

| Field | Default | Description |
|---|---|---|
| `tenant` | `common` | Azure AD tenant. Use `consumers` for personal OneDrive, `common` for any account, or a specific tenant GUID. |
| `client_id` | — | **Required.** Azure app registration client ID. |
| `local_path` | — | **Required.** Local directory to sync. |
| `download_from_cloud` | `true` | Pull changes from OneDrive to local. |
| `upload_from_local` | `true` | Push local changes to OneDrive. |
| `sync_list_path` | — | Path to a text-based sync list file (see [Selective Sync](#-selective-sync)). Ignored if `sync:` section is present. |
| `download_workers` | `8` | Parallel download goroutines. |
| `upload_workers` | `8` | Parallel upload goroutines. |
| `upload_chunk_mb` | `8` | Chunk size in MB for resumable uploads (files >4 MB). Rounded to nearest 320 KiB boundary. |
| `upload_parallel` | `2` | Parallel chunk workers per large file (capped at 4). |
| `interactive` | `false` | When `true`, prompts on conflict instead of auto-selecting "keep both". |
| `log.list_checked` | `false` | Log every file inspected during upload scan. |
| `log.verbose` | `false` | Log every individual sync action. |

### Environment variable overrides

Every key can also be set via environment variable (takes precedence over the config file):

| Variable | Config field |
|---|---|
| `DRIFTSYNC_TENANT` | `tenant` |
| `DRIFTSYNC_CLIENT_ID` | `client_id` |
| `DRIFTSYNC_LOCAL_PATH` | `local_path` |
| `DRIFTSYNC_SYNC_LIST` | `sync_list_path` |
| `DRIFTSYNC_DOWNLOAD_WORKERS` | `download_workers` |
| `DRIFTSYNC_UPLOAD_WORKERS` | `upload_workers` |
| `DRIFTSYNC_UPLOAD_CHUNK_MB` | `upload_chunk_mb` |
| `DRIFTSYNC_UPLOAD_PARALLEL` | `upload_parallel` |

---

## ▶️ Running the Sync

```bash
./driftsync --config ./config.yaml
```

On the first run you will be shown a device-code login prompt. Complete authentication in your browser; the token is saved automatically for subsequent runs.

### CLI flags

| Flag | Alias | Description |
|---|---|---|
| `--config <path>` | | Path to `config.yaml` (default: `config.yaml` in current directory) |
| `--interactive` | `-i` | Enable interactive conflict resolution (overrides config) |
| `--version` | | Print version and exit |
| `--help` | | Show usage |

---

## 📁 Selective Sync

You can restrict which files participate in sync using either a YAML section in the config or a separate text file. If both are present, the `sync:` YAML section takes priority.

**Exclude rules always take priority over include rules.**

### Option 1 — YAML `sync:` section (recommended)

Add a `sync:` block to your `config.yaml`:

```yaml
sync:
  include:
    - /docs          # only the `docs` directory at the root
    - "*.md"         # any .md file anywhere in the tree
    - images         # any directory named `images` at any depth
  exclude:
    - /docs/work.json     # exclude a specific file inside an included dir
    - "*.tmp"
    - /build
    - node_modules
    - obsidian/.obsidian/ # exclude a hidden config subfolder
```

### Option 2 — text sync-list file

Set `sync_list_path` in your config to point to a plain-text file. Each line is a rule:

- Lines starting with `-` or `!` are **exclude** rules; all others are **include** rules.
- A leading `/` anchors the rule to the sync root (prefix match).
- No leading `/` matches the pattern **anywhere** in the tree (supports `*` and `?` wildcards).
- Lines starting with `#` or `;` are comments.

```
/docs
/projects
-*.tmp
-/docs/work.json
-node_modules
```

### How matching works

| Pattern | Matches |
|---|---|
| `/docs` | `docs/` subtree at the root |
| `docs` | any directory named `docs` at any depth |
| `*.md` | any `.md` file anywhere |
| `/docs/work.json` | only that exact file |
| `node_modules` | any `node_modules/` directory at any depth |
| `obsidian/.obsidian/` | the `.obsidian/` subdirectory inside `obsidian/` |

---

## 🗃 Database & State

The SQLite database is stored alongside your config file:

```
config.yaml
driftsync.db
```

The database holds:
- Per-file SHA-256 hash, size, mtime, and ETag
- The OneDrive Delta API token (enables incremental sync on next run)
- OAuth tokens

Do not delete `driftsync.db` unless you want to force a full re-sync on the next run.

---

## 🗑 Trash & Safety

Files deleted from OneDrive or locally are moved to `.driftsync_trash/` inside `local_path` before being removed. They are **not** permanently deleted. You can manually clean this folder when you are satisfied the deletions were correct.

The trash folder itself is never uploaded to OneDrive.

---

## 📝 License

GNU General Public License v3.0 — see [LICENSE](LICENSE) for details.

---

## 🤝 Contributions

Pull requests and feature suggestions are welcome!
