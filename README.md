# Napster-With-Replication (Go)

A Napster-style P2P file sharing system with centralized indexing, primary/shadow master replication, lease-based write coordination, and automatic file replication across peers. Implemented in Go with a persistent interactive CLI.

The system consists of:
- **server/**: Central index that tracks which peers host which files, answers searches, enforces leases, and plans replication.
- **client/**: Peer node that shares files over HTTP, registers with the server, searches, downloads, performs replication tasks, and supports an interactive shell for continuous operation.

This is a teaching/reference implementation: simple JSON over HTTP, in-memory state, no external dependencies, and designed to run locally or within a LAN.

---

## System Description

- **Centralized Index (Napster model)**: A primary server maintains a mapping of filename → peers that currently host the file. Peers register their presence and file list and send periodic heartbeats.
- **Shadow Master Replication**: A standby shadow server receives real-time state synchronization from the primary via HTTP sync operations. Provides read-only failover for high availability.
- **Lease-Based Consistency**: Per-file write locks (leases) enforce sequential consistency. A peer must acquire a 60-second lease before updating a file to prevent write conflicts.
- **Versioned Files**: Files are tracked with timestamp-based versions. Higher versions are treated as authoritative.
- **Replication Planner**: The server tries to keep at least R copies (replication factor) of each file by assigning replication tasks to peers that don't yet host the file.
- **Heartbeat and Pruning**: Peers send heartbeats every 20 seconds. Peers that don't heartbeat within TTL (45s) are pruned from the index.
- **Interactive CLI**: A persistent shell allows peers to execute commands (`search`, `get`, `update`, `list`) continuously without restarting the client.
- **Peer Nodes**: Each peer runs a small HTTP server to expose files at `/files` and to serve downloads to other peers. Peers also periodically fetch and execute server-provided replication tasks.
- **Communication**: All control-plane calls are JSON over HTTP. Data-plane (file content) is served via plain HTTP GET from peer to peer.

---

## Architecture

### Server Components

- **Primary Server**: Authoritative state holder. Plans replication tasks, enforces leases, tracks peers/files, and synchronizes all state changes to the shadow.
- **Shadow Server**: Standby state mirror receiving sync operations from primary via `POST /sync`. Read-only for clients: accepts only read operations (`GET /search`, `GET /peers`, `GET /healthz`) and rejects client writes (`POST /register`, `POST /announce`, `POST /lease`, `POST /heartbeat`) with `403 Forbidden`.

### Client Components

- **Peer Server**: Hosts a small HTTP server to serve files at `/files/<name>`. Exposes file listings at `GET /files`.
- **Interactive Shell**: Persistent CLI that accepts commands in a loop without restarting the client process.
- **Failover Logic**: Clients are configured with a comma-separated list of servers. If the primary fails (5xx error or timeout), the client automatically retries against the shadow.

### Key Concepts

- **Replication Factor**: Configurable via `NAPSTER_REPLICATION_FACTOR` (default: 2). The server ensures this many copies of each file exist across the network.
- **Peer TTL**: Peers that don't heartbeat within 45 seconds are pruned from the index.
- **Heartbeat Interval**: Clients send heartbeats every 20 seconds to stay active.
- **Leases**: 60-second write locks acquired via `POST /lease` before announcing a higher file version.
- **Versioning**: Servers treat higher versions as authoritative (timestamp-based in client updates).

---

## Directory Structure

```
.
├── server/
│   └── server.go           # Central index server implementation
├── client/
│   └── client.go           # Peer client implementation with interactive CLI
├── bin/
│   ├── server              # Compiled server binary
│   └── client              # Compiled client binary
├── tmp/                    # Default directory for peer file storage
│   ├── peer1/
│   ├── peer2/
│   └── peer3/
└── eval.py                 # Automated evaluation and benchmarking script
```

---

## Requirements

- **Go** (1.18+)
- **Python 3** (Optional, for evaluation script)

---

## Build

```bash
go build -o bin/server ./server
go build -o bin/client ./client
```

---

## Server Configuration

Environment variables:

- `NAPSTER_SERVER_ADDR`: Address to bind (e.g., `:8080`). If empty, server picks a free port.
- `NAPSTER_SHADOW_ADDR`: URL of shadow master (e.g., `http://localhost:8081`). Set only on the primary server.
- `NAPSTER_REPLICATION_FACTOR`: Desired replication factor (default: `2`).

**Examples:**

Shadow-only server:
```bash
NAPSTER_SERVER_ADDR=":8081" ./bin/server
```

Primary server with shadow:
```bash
NAPSTER_SHADOW_ADDR="http://localhost:8081" NAPSTER_SERVER_ADDR=":8080" ./bin/server
```

---

## Client Flags

- `-server`: Comma-separated list of servers (e.g., `http://localhost:8080,http://localhost:8081`)
- `-dir`: Shared directory for peer files (e.g., `tmp/peer1`)
- `-bind`: Bind address for peer server (e.g., `:9001`)
- `-addr`: Public address of peer (e.g., `http://localhost:9001`) — optional if bind is set; auto-fills IP:port
- `-cmd`: Command to execute: `serve`, `search`, `get`, `update`, `list`

---

## HTTP Endpoints and Data Formats

All endpoints return JSON unless noted. Content-Type for JSON requests must be `application/json`.

### Server Endpoints

#### `POST /register`
Register a peer and its files with the server.

**Request:**
```json
{
  "peer": {
    "id": "peer1",
    "addr": "http://localhost:9001",
    "lastSeen": "2025-01-01T12:00:00Z"
  },
  "files": [
    {
      "name": "doc.txt",
      "size": 1024,
      "hash": "",
      "version": 1234567890
    }
  ]
}
```

**Response:**
```json
{
  "ok": true,
  "tasks": [
    {
      "file": { "name": "doc.txt", "size": 1024, "hash": "", "version": 1234567890 },
      "sourcePeer": { "id": "peer1", "addr": "http://localhost:9001", "lastSeen": "..." }
    }
  ]
}
```

**Notes:** Saves/updates peer and files. Returns replication tasks if the replication factor is not met.

#### `POST /heartbeat`
Extend peer liveness timestamp.

**Request:**
```json
{
  "peerId": "peer1"
}
```

**Response:**
```json
{
  "ok": true,
  "tasks": []
}
```

**Notes:** If `ok=false` (peer unknown), client should re-register.

#### `POST /announce`
Announce file ownership (used after downloads or updates).

**Request:**
```json
{
  "peer": { "id": "peer1", "addr": "http://localhost:9001", "lastSeen": "..." },
  "file": { "name": "doc.txt", "size": 1024, "hash": "", "version": 1234567891 }
}
```

**Response:** `204 No Content` on success; `403 Forbidden` if lease required and missing.

**Notes:** If version increases, server verifies that the peer holds a valid lease for the file.

#### `POST /lease`
Acquire a write lock for a file.

**Request:**
```json
{
  "peerId": "peer1",
  "fileName": "doc.txt"
}
```

**Response:**
```json
{
  "granted": true,
  "expiration": "2025-01-01T12:01:00Z",
  "message": "Lease granted"
}
```

**Notes:** Grants a 60-second lease if available. Rejects with holder info if another peer holds the lease.

#### `GET /search?q=<query>`
Search for files matching the query string.

**Response:**
```json
{
  "matches": [
    {
      "file": { "name": "doc.txt", "size": 1024, "hash": "", "version": 1234567890 },
      "peers": [
        { "id": "peer1", "addr": "http://localhost:9001", "lastSeen": "..." }
      ]
    }
  ]
}
```

#### `GET /peers?file=<filename>`
Get all peers hosting a specific file.

**Response:**
```json
{
  "file": { "name": "doc.txt", "size": 1024, "hash": "", "version": 1234567890 },
  "peers": [
    { "id": "peer1", "addr": "http://localhost:9001", "lastSeen": "..." }
  ]
}
```

**Notes:** Returns `404 Not Found` if file doesn't exist.

#### `POST /sync` (Shadow Only)
Receive state synchronization from primary.

**Request:**
```json
{
  "opType": "register",
  "data": { ... }
}
```

**Response:** `200 OK`

**Notes:** Primary pushes operations (`register`, `announce`, `heartbeat`, `prune`). Shadow applies state mutations.

#### `GET /healthz`
Health check endpoint.

**Response:** `200 OK`, body: `ok`

### Shadow Write Policy

The shadow server is strictly read-only for clients. The following endpoints return `403 Forbidden` on the shadow:
- `POST /register`
- `POST /announce`
- `POST /lease`
- `POST /heartbeat`

Clients should direct all writes to the primary. Reads automatically fail over to the shadow when the primary is unavailable.

### Client Endpoints (Peer Servers)

#### `GET /files`
List all files shared by this peer.

**Response:**
```json
[
  { "name": "doc.txt", "size": 1024, "hash": "", "version": 1234567890 }
]
```

#### `GET /files/<name>`
Download a file from this peer.

**Response:** File content stream (`200 OK`), or `404 Not Found`.

---

## Data Structures

### `Peer`
```json
{
  "id": "peer1",
  "addr": "http://localhost:9001",
  "lastSeen": "2025-01-01T12:00:00Z"
}
```

### `FileInfo`
```json
{
  "name": "doc.txt",
  "size": 1024,
  "hash": "",
  "version": 1234567890
}
```

**Notes:** Hash field is reserved for future use. Version is a timestamp for consistency tracking.

### `ReplicationTask`
```json
{
  "file": { "name": "doc.txt", "size": 1024, "hash": "", "version": 1234567890 },
  "sourcePeer": { "id": "peer1", "addr": "http://localhost:9001", "lastSeen": "..." }
}
```

### `SyncOp`
```json
{
  "opType": "register",
  "data": { ... }
}
```

**opType values:** `register`, `announce`, `heartbeat`, `prune`

---

## Usage Instructions

### Scenario A: Local Testing (Single Laptop)

Run everything on `localhost` using different ports.

**1. Start the Server Cluster**

Terminal 1 — Shadow Server:
```bash
NAPSTER_SERVER_ADDR=":8081" ./bin/server
```

Terminal 2 — Primary Server:
```bash
NAPSTER_SHADOW_ADDR="http://localhost:8081" NAPSTER_SERVER_ADDR=":8080" ./bin/server
```

**2. Start Peers**

Terminal 3 — Peer 1:
```bash
mkdir -p tmp/peer1 && echo "Content v1" > tmp/peer1/doc.txt
./bin/client -cmd serve -server "http://localhost:8080,http://localhost:8081" \
             -dir tmp/peer1 -bind :9001 -addr http://localhost:9001
```

Terminal 4 — Peer 2:
```bash
mkdir -p tmp/peer2
./bin/client -cmd serve -server "http://localhost:8080,http://localhost:8081" \
             -dir tmp/peer2 -bind :9002 -addr http://localhost:9002
```

Terminal 5 — Peer 3:
```bash
mkdir -p tmp/peer3
./bin/client -cmd serve -server "http://localhost:8080,http://localhost:8081" \
             -dir tmp/peer3 -bind :9003 -addr http://localhost:9003
```

**3. Observe Logs**

Watch for registration, heartbeat, replication tasks, and announce operations in the server and client logs.

**4. Interactive CLI Commands**

Once a peer is running in serve mode, the interactive shell accepts the following commands:

| Command | Usage | Description |
|---------|-------|-------------|
| `list` | `list` | Shows files currently in your local shared folder. |
| `search` | `search <query>` | Asks the server who has a file. Returns a list of peers. |
| `get` | `get <filename>` | Downloads the file from a peer and announces it to the server. |
| `update` | `update <filename>` | Acquires a write lease, opens editor, user edits and saves, and publishes version v+1. |
| `help` | `help` | Displays the list of available commands. |
| `exit` | `exit` | Closes the client. |

**Example Session:**
```
> list
Files in tmp/peer2:
  doc.txt (12 bytes)

> search doc
Found 1 matches:
  doc.txt (12 bytes, v1234567890)
    Hosted by: peer1 (http://localhost:9001)

> get doc.txt
Downloading doc.txt from peer1...
Download complete. Announced to server.

> update doc.txt
Acquiring lease for doc.txt...
Lease granted. Updating file...
Update complete. Version: 1234567900

> exit
Goodbye!
```

### Scenario B: Distributed Testing (Multiple Laptops)

Run servers on one machine and peers on different machines connected to the same Wi-Fi/LAN.

**1. Setup Server Machine (e.g., IP: 192.168.1.10)**

Terminal 1 — Shadow Server:
```bash
NAPSTER_SERVER_ADDR=":8081" ./bin/server
```

Terminal 2 — Primary Server:
```bash
NAPSTER_SHADOW_ADDR="http://192.168.1.10:8081" NAPSTER_SERVER_ADDR=":8080" ./bin/server
```

**Note:** `:8080` and `:8081` bind to all network interfaces, so they are accessible from other laptops.

**2. Setup Laptop 2 (Peer A) (e.g., IP: 192.168.1.20)**

```bash
mkdir -p tmp/peerA && echo "Content from Laptop 2" > tmp/peerA/doc.txt
./bin/client -cmd serve \
             -server "http://192.168.1.10:8080,http://192.168.1.10:8081" \
             -dir tmp/peerA \
             -bind :9001 \
             -addr http://192.168.1.20:9001
```

**Note:** `-addr` must be THIS laptop's IP so other peers can download from it.

**3. Setup Laptop 3 (Peer B) (e.g., IP: 192.168.1.30)**

```bash
mkdir -p tmp/peerB
./bin/client -cmd serve \
             -server "http://192.168.1.10:8080,http://192.168.1.10:8081" \
             -dir tmp/peerB \
             -bind :9001 \
             -addr http://192.168.1.30:9001
```

**4. Firewall Configuration**

Ensure firewall rules allow inbound connections on:
- Server ports: `8080`, `8081`
- Peer ports: `9001`, `9002`, `9003`, etc.

**5. Testing**

Use the interactive CLI on any peer to search, get, and update files across the distributed network.

---

## CLI Commands

The client supports several commands via the `-cmd` flag or through the interactive shell.

### `serve` — Run as a Peer Node
Starts the peer in server mode: registers with the central server, shares files, sends heartbeats, executes replication tasks, and provides an interactive shell.

```bash
./bin/client -cmd serve -server http://localhost:8080,http://localhost:8081 \
             -dir tmp/peer1 -bind :9001 -addr http://localhost:9001
```

### `search <query>` — Find Files
Searches the network for files matching the query string.

```bash
./bin/client -cmd search -server http://localhost:8080 -dir tmp/peer1 doc
```

**Output:** Lists matching files and which peers host them.

### `get <filename>` — Download a File
Downloads a file from the network and saves it to your local directory.

```bash
./bin/client -cmd get -server http://localhost:8080 -dir tmp/peer1 doc.txt
```

**Note:** The file is pulled from a peer and announced to the server.

### `list` — List Local Files
Shows all files in your shared directory.

```bash
./bin/client -cmd list -server http://localhost:8080 -dir tmp/peer1
```

### `update <filename>` — Update a File (Requires Write Lock)
Acquires a lease, then announces a higher version to coordinate writes across peers.

```bash
./bin/client -cmd update -server http://localhost:8080 -dir tmp/peer1 doc.txt
```

---

## Quick Start (3 Terminals)

**Terminal 1 — Start Shadow Server:**
```bash
NAPSTER_SERVER_ADDR=":8081" ./bin/server
```

**Terminal 2 — Start Primary Server:**
```bash
NAPSTER_SHADOW_ADDR="http://localhost:8081" NAPSTER_SERVER_ADDR=":8080" ./bin/server
```

**Terminal 3 — Start Peer with Interactive Shell:**
```bash
mkdir -p tmp/peer1 && echo "Content v1" > tmp/peer1/doc.txt
./bin/client -cmd serve -server "http://localhost:8080,http://localhost:8081" \
             -dir tmp/peer1 -bind :9001 -addr http://localhost:9001
```

In the interactive shell, try:
```
> list
> search doc
> help
> exit
```

---

## Comprehensive Manual Test Cases

### 1. Registration and Initial Replication
**Setup:** Peer1 has `doc.txt`; Peer2 and Peer3 are empty.

**Action:** Start servers, start peers.

**Expect:** Primary logs `[register]` and replication planning. Peer2 pulls `doc.txt` from Peer1 and announces. Peer3 may pull if replication factor demands.

### 2. Heartbeat Stability
**Action:** Observe client logs every ~20s; verify server `[heartbeat] ok=true` and no `[prune]` for active peers.

**Edge Case:** Temporarily stop Peer2 for >45s; expect `[prune]` on server removing Peer2 and its file mapping.

### 3. Failover to Shadow Master
**Action:** Shut down primary temporarily; clients continue using shadow for reads and lookups.

**Expect:** Client logs show retries and successes against the shadow. `[shadow] handleSync` continues to apply operations when primary is back.

### 4. Lease Enforcement on Update
**Action:** On Peer1, run `update doc.txt` in the interactive shell.

**Expect:** `[lease] granted` then `[announce] ok` with increasing version.

**Edge Case:** Try announcing a higher version from Peer2 without lease; server returns `403` and logs `[announce] rejected: lease missing/expired`.

### 5. Search and Get
**Action:** Run `search doc` in the interactive shell; expect matches with peers listed.

**Action:** On Peer3, run `get doc.txt`; expect download from one listed peer and subsequent announce.

### 6. Stale Prune Correctness
**Action:** Start Peer2, then kill it; after TTL (~45s), server `prune` logs show removal.

**Expect:** Files hosted solely by Peer2 are unlisted in searches.

### 7. Replication Factor
**Setup:** Set `NAPSTER_REPLICATION_FACTOR=3` on primary. Start 3 peers.

**Action:** Register peers; server logs show planning for all files to reach factor 3; tasks enqueued accordingly.

### 8. Multi-Host LAN Scenario
**Action:** Run servers on two laptops (shadow and primary) with LAN IPs; start peers across machines.

**Expect:** Registration and replication work across network; search/get/update function with public addresses; logs indicate cross-host pulls.

### 9. Error Handling and Failover Paths
**Action:** Temporarily block one server; client logs `[client] request ... -> will try next` and then success with the other.

**Edge Case:** Send bad JSON to server endpoints (use curl) to see `[bad json]` logs and 400 responses.

### 10. Large Number of Peers/Files (Sanity)
**Action:** Run multiple peer processes on one machine with different dirs; add several files; observe replication planning and queue behavior in logs.

### 11. Interactive CLI Persistence
**Action:** Start a peer in serve mode and execute multiple commands without restarting.

**Expect:** Shell remains active. Commands like `list`, `search`, `get`, `update` execute sequentially without client restart.

### 12. Shadow Read-Only Policy
**Action:** Use curl to send `POST /register` directly to the shadow server.

**Expect:** `403 Forbidden` response. Client logs should show automatic retry against the primary.

---

## Evaluation and Metrics

The system includes an evaluation suite (`eval.py`) to benchmark performance.

**Metrics:**
- **Throughput:** Requests processed per second.
- **Latency:** Time taken for `register`, `search`, and `download`.
- **Scalability:** Performance behavior as peer count increases (10, 50, 100 peers).
- **Failover Time:** Time taken to switch from primary to shadow during failure.
- **Replication Efficiency:** Time taken to achieve replication factor across peers.

**Running the Evaluation:**
```bash
python3 eval.py
```

**Automated Tests:**
- Shadow Read-Only Policy: Direct `POST` requests to the shadow for `/register`, `/announce`, `/lease`, `/heartbeat` should return `403`.
- Failover with Shadow Read-Only: Crash primary, confirm shadow rejects writes; restart primary, confirm writes succeed again.

---

## Troubleshooting

**Peer Pruned Unexpectedly:**
- Confirm heartbeats are sent every ~20s.
- Check that `LastSeen` is being updated in server logs.
- Ensure `[announce]` logs don't overwrite `LastSeen` with zero.

**Lease Update Fails:**
- Ensure you're using the `update` command which requests a lease before announcing higher versions.
- Check server logs for `[lease]` entries.

**Multi-Laptop Setup Issues:**
- Verify IP addressing: use `ifconfig` or `ipconfig` to confirm LAN IPs.
- Check firewall rules on both peers and servers.
- Test connectivity with `curl http://<server-ip>:8080/healthz`.

**Downloads Timeout:**
- Client download uses a 10-second timeout. Adjust if pulling large files over slow networks.
- Check that peer servers are listening on the correct ports.

**Shadow Not Syncing:**
- Confirm `NAPSTER_SHADOW_ADDR` is set correctly on the primary.
- Check primary logs for `[sync]` entries.
- Verify shadow logs show `[shadow] handleSync` operations.

---