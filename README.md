# Napster-With-Replication (Go)

A minimal Napster-style P2P file sharing system with a central index server and basic file replication across peers, implemented in Go. The system consists of:

- server/: Central index that tracks which peers host which files, answers searches, and plans replication.
- client/: Peer node that shares files over HTTP, registers with the server, searches, downloads, and performs replication tasks.

This is a teaching/reference implementation: simple JSON over HTTP, in-memory state, no external dependencies, and designed to run locally or within a LAN.

## System description

- Centralized Index (Napster model): A single server maintains a mapping of filename -> peers that currently host the file. Peers register their presence and file list and send periodic heartbeats.
- Replication Planner: The server tries to keep at least R copies (replication factor) of each file by assigning replication tasks to peers that don't yet host the file.
- Peer Nodes: Each peer runs a small HTTP server to expose files at /files and to serve downloads to other peers. Peers also periodically fetch and execute server-provided replication tasks.
- Communication: All control-plane calls are JSON over HTTP. Data-plane (file content) is served via plain HTTP GET from peer to peer.
# Napster-With-Replication

A simple Napster-style file sharing system in Go with:

- Primary/Shadow master replication (state sync via HTTP)
- Lease-based write coordination (per-file write locks)
- Versioned consistency (timestamp versions)
- Heartbeat and stale peer pruning
- Search and peer discovery
- Automatic replication to satisfy a replication factor
- Client-side failover across multiple servers

This README explains architecture, endpoints, data formats, how to run locally and across multiple laptops, and comprehensive manual test cases.

---

## Architecture

- Server (Primary): authoritative state holder. Plans replication tasks, enforces leases, tracks peers/files.
- Server (Shadow): standby state mirror receiving sync ops from primary (register/announce/heartbeat/prune). Read-only: rejects client write operations (`POST /register`, `POST /announce`, `POST /lease`, `POST /heartbeat`). Only accepts `POST /sync` from the Primary and serves read endpoints (`GET /search`, `GET /peers`, `GET /healthz`).
- Client (Peer): hosts a small HTTP server to serve files. Registers with the master(s), heartbeats, pulls replication tasks, and announces file ownership. Supports update via lease.

Key concepts:
- Replication factor: configurable via `NAPSTER_REPLICATION_FACTOR` (default 2).
- Peer TTL: peers that don't heartbeat within TTL (45s) are pruned.
- Heartbeats: clients send heartbeats every 20s to stay under TTL.
- Leases: a peer acquires a lease before announcing a higher file version.
- Versioning: servers treat higher version as authoritative (timestamp-based in client updates).

---

## Binaries and layout

- `./bin/server`: server binary
- `./bin/client`: client binary
- Client shares files from a directory (e.g. `tmp/peer1`)

---

## Server configuration

Environment variables:
- `NAPSTER_SERVER_ADDR`: address to bind (e.g. `:8080`). If empty, server picks a free port.
- `NAPSTER_SHADOW_ADDR`: URL of shadow master (`http://host:port`). Set only on the primary.
- `NAPSTER_REPLICATION_FACTOR`: desired replication factor (default: `2`).

Examples:
- Shadow-only server: `NAPSTER_SERVER_ADDR=":8081" ./bin/server`
- Primary server with shadow: `NAPSTER_SHADOW_ADDR="http://localhost:8081" NAPSTER_SERVER_ADDR=":8080" ./bin/server`

---

## Client flags

- `-server`: comma-separated list of servers (e.g., `http://localhost:8080,http://localhost:8081`)
- `-dir`: shared directory (e.g., `tmp/peer1`)
- `-bind`: bind address for peer server (e.g., `:9001`)
- `-addr`: public address of peer (e.g., `http://localhost:9001`) — optional if bind is set; auto-fills IP:port.
- `-cmd`: `serve`, `search`, `get`, `update`, `list`

---

## HTTP Endpoints and Data Formats

All endpoints return JSON unless noted. Content-Type for JSON requests must be `application/json`.

### Server endpoints

- `POST /register`
	- Request: `{ "peer": Peer, "files": [FileInfo] }`
	- Response: `{ "ok": true, "tasks": [ReplicationTask] }`
	- Notes: Saves/updates peer and files; planning replication tasks returned to requester.

- `POST /heartbeat`
	- Request: `{ "peerId": string }`
	- Response: `{ "ok": boolean, "tasks": [ReplicationTask] }`
	- Notes: Extends `LastSeen`. If `ok=false` (peer unknown), client should re-register.

- `POST /announce`
	- Request: `{ "peer": Peer, "file": FileInfo }`
	- Response: `204 No Content` on success; `403 Forbidden` if lease required and missing.
	- Notes: Used after replication pulls and updates. If version increases, server verifies lease.

- `POST /lease`
	- Request: `{ "peerId": string, "fileName": string }`
	- Response: `{ "granted": boolean, "expiration": RFC3339 time, "message": string }`
	- Notes: Grants 60s lease if available; reject with holder info otherwise.

- `GET /search?q=<query>`
	- Response: `{ "matches": [ { "file": FileInfo, "peers": [Peer] } ] }`

- `GET /peers?file=<filename>`
	- Response: `{ "file": FileInfo, "peers": [Peer] }` or `404 Not Found`.

- `POST /sync` (shadow only)
	- Request: `{ "opType": "register"|"announce"|"heartbeat"|"prune", "data": RawJSON }`
	- Response: `200 OK`
	- Notes: Primary pushes operations; shadow applies state mutations.

Shadow write policy:
- Shadow server is strictly read-only for clients. The following endpoints return `403 Forbidden` on the shadow: `POST /register`, `POST /announce`, `POST /lease`, `POST /heartbeat`.
- Clients should direct all writes to the Primary. Reads will fail over to the Shadow when needed.

### New automated tests in `eval.py`

- Shadow Read-Only Policy: Direct `POST` requests to the shadow for `/register`, `/announce`, `/lease`, `/heartbeat` should return `403`.
- Failover with Shadow Read-Only: Crash primary, confirm shadow rejects writes; restart primary, confirm writes succeed again.

### Implemented/affected server functions

- `handleRegister` (server.go): rejects on shadow with `403`, plans replication on primary.
- `handleAnnounce` (server.go): rejects on shadow with `403`, enforces lease on primary.
- `handleLease` (server.go): rejects on shadow with `403`, grants 60s lease on primary.
- `handleHeartbeat` (server.go): rejects on shadow with `403`; heartbeat state applied to shadow via `/sync` from primary.
- `handleSync` (server.go): shadow applies primary's operations.

- `GET /healthz`
	- Response: `200 OK`, body `ok`

### Client endpoints (peer servers)

- `GET /files`
	- Response: `[FileInfo]`

- `GET /files/<name>`
	- Response: file content stream (`200 OK`), or `404 Not Found`

### Data structures

`Peer`:
```
{
	"id": string,
	"addr": string,   // http://host:port
	"lastSeen": RFC3339 time
}
```

`FileInfo`:
```
{
	"name": string,
	"size": number,
	"hash": string,   // not currently set by client, reserved for future use
	"version": number // timestamp version used for consistency
}
```

`ReplicationTask`:
```
{
	"file": FileInfo,
	"sourcePeer": Peer
}
```

`RegisterResponse`:
```
{ "ok": true, "tasks": [ReplicationTask] }
```

`HeartbeatResponse`:
```
{ "ok": boolean, "tasks": [ReplicationTask] }
```

`LeaseRequest`/`LeaseResponse`:
```
// Request
{ "peerId": string, "fileName": string }

// Response
{ "granted": boolean, "expiration": string, "message": string }
```

`SyncOp`:
```
{ "opType": "register"|"announce"|"heartbeat"|"prune", "data": RawJSON }
```

---

## Build

```
go build -o bin/server ./server
go build -o bin/client ./client
```

---

## Run locally on a single laptop (localhost)

1) Start shadow server:
```
NAPSTER_SERVER_ADDR=":8081" ./bin/server
```

2) Start primary server with shadow:
```
NAPSTER_SHADOW_ADDR="http://localhost:8081" NAPSTER_SERVER_ADDR=":8080" ./bin/server
```

3) Start peers:
```
mkdir -p tmp/peer1 && echo "Content v1" > tmp/peer1/doc.txt
./bin/client -cmd serve -server "http://localhost:8080,http://localhost:8081" -dir tmp/peer1 -bind :9001 -addr "http://localhost:9001"

mkdir -p tmp/peer2
./bin/client -cmd serve -server "http://localhost:8080,http://localhost:8081" -dir tmp/peer2 -bind :9002 -addr "http://localhost:9002"

mkdir -p tmp/peer3
./bin/client -cmd serve -server "http://localhost:8080,http://localhost:8081" -dir tmp/peer3 -bind :9003 -addr "http://localhost:9003"
```

4) Observe logs for registration, heartbeat, replication tasks, and announce.

5) Search and get:
```
./bin/client -cmd search -server "http://localhost:8080,http://localhost:8081" -dir tmp/peer2 -bind :9002 -addr "http://localhost:9002" doc
./bin/client -cmd get -server "http://localhost:8080,http://localhost:8081" -dir tmp/peer3 -bind :9003 -addr "http://localhost:9003" doc.txt
```

6) Update with lease (on peer1):
```
./bin/client -cmd update -server "http://localhost:8080,http://localhost:8081" -dir tmp/peer1 -bind :9001 -addr "http://localhost:9001" doc.txt
```
This will acquire a lease and announce a higher version.

---

## Run across multiple laptops

Assume Laptop A (shadow) and Laptop B (primary) are on the same LAN.

1) On Laptop A (Shadow):
- Find LAN IP (e.g., `192.168.1.10`).
```
NAPSTER_SERVER_ADDR=":8081" ./bin/server
```
Take note of public URL printed (e.g., `http://192.168.1.10:8081`).

2) On Laptop B (Primary):
- Find LAN IP (e.g., `192.168.1.11`).
```
NAPSTER_SHADOW_ADDR="http://192.168.1.10:8081" NAPSTER_SERVER_ADDR=":8080" ./bin/server
```
Take note of public URL printed (e.g., `http://192.168.1.11:8080`).

3) On any laptops running peers (A or B):
- Use `-server` listing both servers with their LAN IPs:
```
./bin/client -cmd serve \
	-server "http://192.168.1.11:8080,http://192.168.1.10:8081" \
	-dir tmp/peerX \
	-bind :900X \
	-addr "http://<this_laptop_ip>:900X"
```
- Ensure firewall rules allow inbound on peer ports (900X) and servers (8080/8081).

4) Testing is the same: search, get, update using the public addresses.

---

## Comprehensive manual test cases

1) Registration and initial replication
- Setup: Peer1 has `doc.txt`; Peer2 and Peer3 are empty.
- Action: Start servers, start peers.
- Expect: Primary logs `[register]` and replication planning; Peer2 pulls `doc.txt` from Peer1 and announces; Peer3 may pull if replication factor demands.

2) Heartbeat stability
- Action: Observe client logs every ~20s; verify server `[heartbeat] ok=true` and no `[prune]` for active peers.
- Edge: Temporarily stop Peer2 for >45s; expect `[prune]` on server removing Peer2 and its file mapping.

3) Failover to shadow master
- Action: Shut down primary temporarily; clients continue using shadow for reads and lookups.
- Expect: Client logs show retries and successes against the shadow; `[shadow] handleSync` continues to apply operations when primary is back.

4) Lease enforcement on update
- Action: On Peer1, run `update doc.txt`.
- Expect: `[lease] granted` then `[announce] ok` with increasing version.
- Edge: Try announcing a higher version from Peer2 without lease; server returns `403` and logs `[announce] rejected: lease missing/expired`.

5) Search and Get
- Action: Run `search doc`; expect matches with peers listed.
- Action: On Peer3, run `get doc.txt`; expect download from one listed peer and subsequent announce.

6) Stale prune correctness
- Action: Start Peer2, then kill it; after TTL (~45s), server `prune` logs show removal. Ensure files hosted solely by Peer2 are unlisted in searches.

7) Replication factor
- Setup: Set `NAPSTER_REPLICATION_FACTOR=3` on primary. Start 3 peers.
- Action: Register peers; server logs show planning for all files to reach factor 3; tasks enqueued accordingly.

8) Multi-host LAN scenario
- Action: Run servers on two laptops (shadow and primary) with LAN IPs; start peers across machines.
- Expect: Registration and replication work across network; search/get/update function with public addresses; logs indicate cross-host pulls.

9) Error handling and failover paths
- Action: Temporarily block one server; client logs `[client] request ... -> will try next` and then success with the other.
- Edge: Send bad JSON to server endpoints (use curl) to see `[bad json]` logs and 400 responses.

10) Large number of peers/files (sanity)
- Action: Run multiple peer processes on one machine with different dirs; add several files; observe replication planning and queue behavior in logs.

---

## Troubleshooting

- If a peer is pruned unexpectedly, confirm heartbeats every ~20s and that `LastSeen` is being updated; check `[announce]` logs to ensure `LastSeen` isn’t overwritten with zero.
- If lease updates fail, ensure you’re using `-cmd update` which requests a lease before announcing higher versions.
- For multi-laptop setups, verify IP addressing and firewall rules on both peers and servers.
- Use `GET /healthz` on servers to confirm they’re up.

---

## Notes

- Hash field in `FileInfo` is currently unused; you can extend the client to compute hashes to improve content integrity.
- Shadow master pruning is simplified; the primary is authoritative. Heartbeats are synced to shadow to reduce accidental stale pruning.
- Timeouts: client download uses a 10s timeout; adjust if pulling large files over slow networks.


## CLI Commands

The client supports several commands via the `-cmd` flag. Each command requires a server and directory.

### `serve` — Run as a peer node
Starts the peer in server mode: registers with the central server, shares files, sends heartbeats, and executes replication tasks.

```bash
./bin/client -cmd serve -server http://localhost:8080 -dir tmp/peer1 -bind :9001 -addr http://localhost:9001
```

### `search <query>` — Find files
Searches the network for files matching the query string.

```bash
./bin/client -cmd search -server http://localhost:8080 -dir tmp/peer1 doc
```
Output: Lists matching files and which peers host them.

### `get <filename>` — Download a file
Downloads a file from the network and saves it to your local directory.

```bash
./bin/client -cmd get -server http://localhost:8080 -dir tmp/peer1 doc.txt
```
The file is pulled from a peer and announced to the server.

### `list` — List local files
Shows all files in your shared directory.

```bash
./bin/client -cmd list -server http://localhost:8080 -dir tmp/peer1
```

### `update <filename>` — Update a file (requires write lock)
Acquires a lease, then announces a higher version to coordinate writes across peers.

```bash
./bin/client -cmd update -server http://localhost:8080 -dir tmp/peer1 doc.txt
```

---

## Quick Start (3 terminals)

**Terminal 1 — Start Shadow Server:**
```bash
NAPSTER_SERVER_ADDR=":8081" ./bin/server
```

**Terminal 2 — Start Peer 1:**
```bash
mkdir -p tmp/peer1 && echo "Content v1" > tmp/peer1/doc.txt
./bin/client -cmd serve -server http://localhost:8081 -dir tmp/peer1 -bind :9001 -addr http://localhost:9001
```

**Terminal 3 — Start Peer 2:**
```bash
mkdir -p tmp/peer2
./bin/client -cmd serve -server http://localhost:8081 -dir tmp/peer2 -bind :9002 -addr http://localhost:9002
```

Then in another terminal, try commands like:
```bash
./bin/client -cmd search -server http://localhost:8081 -dir tmp/peer2 doc
./bin/client -cmd get -server http://localhost:8081 -dir tmp/peer2 doc.txt
./bin/client -cmd list -server http://localhost:8081 -dir tmp/peer2
```

---