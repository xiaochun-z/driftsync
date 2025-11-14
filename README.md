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
- Keeps a persistent local database (`driftsync.db`)  
- Stores hash, timestamps, and delta tokens  
- Ensures safe resume and consistent sync

### 🔐 Secure Authentication (Device Code Flow)
- Native Microsoft identity login  
- Tokens securely stored locally  

### 🚀 Lightweight & Cross‑Platform
- Written in Go  
- Runs on Linux, macOS, Windows, WSL, Dev Containers  
- No external runtime dependencies

### 📁 Selective Sync Support
- Ability to **include** or **exclude** specific paths  
- Useful for ignoring large or irrelevant directories  
- Perfect for code repos, workspaces, or partial sync needs

---

## 📦 Installation

Download the latest release for your platform from GitHub (or build from source):

```bash
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o driftsync.exe ./cmd/driftsync
```

Linux build:

```bash
go build -o driftsync ./cmd/driftsync
```

---

## ⚙️ Configuration

Create a configuration file (default: `config.yaml`):

```yaml
tenant: "<your-azure-tenant-id>"
clientId: "<your-azure-app-client-id>"
root: "/work/data"

include:
  - "documents"
  - "projects"

exclude:
  - ".git"
  - "node_modules"
```

### Explanation

| Key         | Description |
|-------------|-------------|
| `tenant`    | Azure AD tenant ID (GUID) |
| `clientId`  | Application ID registered for OneDrive API |
| `root`      | Local folder to sync |
| `include`   | Optional: only sync these relative paths |
| `exclude`   | Optional: skip these paths |

---

## 🎯 Selective Sync

Selective sync lets you control which parts of the local folder participate in synchronization.

### Example: Include only specific folders

```yaml
include:
  - "src"
  - "docs"
```

Meaning: *Only these two directories will sync. Everything else will be skipped.*

---

### Example: Exclude large or unwanted directories

```yaml
exclude:
  - "node_modules"
  - "bin"
  - "obj"
  - ".cache"
```

Meaning: *These paths are ignored even if included elsewhere.*

---

### How Matching Works

- All paths are matched relative to `root`
- Matching is prefix‑based  
  Example:  
  `exclude: ["node_modules"]` will ignore all:

```
node_modules/
project/node_modules/
any/path/node_modules/
```

- Include rules override exclude rules (include has priority)

---

## ▶️ Running the Sync

```bash
./driftsync --config ./config.yaml
```

You will be prompted for device‑code login on the first run.

---

## 🔧 Optional Flags

| Flag | Description |
|------|-------------|
| `--config` | Path to `config.yaml` |
| `--version` | Print version |
| `--help` | Show usage and options |

---

## 🗃 Database

The SQLite database is stored next to your config file:

```
config.yaml
driftsync.db
```

This allows reliable incremental sync between runs.

---

## 📝 License

Apache 2.0 / MIT (choose your preferred license)

---

## 🤝 Contributions

Pull requests and feature suggestions are welcome!