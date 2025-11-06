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

## Features implemented

- File registration: Peers scan a shared directory and register their file list with the server.
- Search: Query by substring to discover files and peer addresses hosting them.
- Download: Fetch a file directly from a listed peer via HTTP and announce the new copy to the server.
- Replication: Server enforces a minimum replication factor by issuing pull tasks to peers; peers execute by downloading from a chosen source.
- Peer liveness: Heartbeats refresh liveness; stale peers are pruned and their files removed from the index.
- Simple, modular layout: server/ and client/ in separate main packages with shared types in pkg/shared/.

Out of scope for this baseline: NAT traversal, firewalls, authentication, integrity verification, chunking/swarming, and persistence beyond process lifetime.

## Requirements

- Go 1.21+
- Linux/macOS/Windows (tested primarily on Linux)

## Build

Build both binaries from the repo root:

```
go build -o bin/server ./server
go build -o bin/client ./client
```

## Run

1) Start the central server (default :8080):

```
NAPSTER_REPLICATION_FACTOR=2 ./bin/server
```

Optional env:

- NAPSTER_SERVER_ADDR: listen address (default ":8080").
- NAPSTER_REPLICATION_FACTOR: minimum copies per file (default 2).

2) Start two peers in separate terminals, each with its own shared directory and public address:

```
./bin/client -cmd serve -server http://localhost:8080 -dir /tmp/peer1 -addr http://localhost:9000 -bind :9000 &
./bin/client -cmd serve -server http://localhost:8080 -dir /tmp/peer2 -addr http://localhost:9001 -bind :9001 &
```

Place some files in /tmp/peer1 and watch peer2 replicate after a short delay (depends on replication factor and task scheduling).

3) Search for files:

```
./bin/client -cmd search -server http://localhost:8080 -q some-name
```

4) Download a specific file by name into your peer's shared dir and announce it:

```
./bin/client -cmd get -server http://localhost:8080 -dir /tmp/peer2 -addr http://localhost:9001 -file example.txt
```

5) List files you currently share:

```
./bin/client -cmd list -dir /tmp/peer2
```

### Run across laptops (same network)

- Start server listening on all interfaces (default) on the machine acting as tracker:
	- Example on server host with IP 192.168.1.10:
		- `NAPSTER_SERVER_ADDR=:8080 ./bin/server`
	- Other laptops should use `-server http://192.168.1.10:8080`.
- On each laptop, run the client and set:
	- `-addr` to the publicly reachable base URL of that laptop (e.g., `http://192.168.1.11:9000`). This is what others use to download from you.
	- `-bind` to the local listen address, usually `:9000` or `0.0.0.0:9000` to listen on all interfaces.

Example on Laptop A (192.168.1.11):

```
./bin/client -cmd serve -server http://192.168.1.10:8080 -dir /home/user/peerA -addr http://192.168.1.11:9000 -bind :9000
```

Example on Laptop B (192.168.1.12):

```
./bin/client -cmd serve -server http://192.168.1.10:8080 -dir /home/user/peerB -addr http://192.168.1.12:9001 -bind :9001
```

Ensure the machines are on the same network and routable. NAT traversal and firewall configuration are out of scope for this baseline.

## How to test across laptops

Follow this quick checklist to verify everything end-to-end on a LAN.

1) Prereqs
- Machines are on the same network (e.g., 192.168.1.x).
- Choose ports: server 8080, peers 9000/9001 (or others).
- Ensure these ports are allowed on each OS (if a firewall is enabled, allow inbound 8080 and the chosen peer ports).

2) Start the server on the tracker machine (e.g., 192.168.1.10)

```
NAPSTER_REPLICATION_FACTOR=2 NAPSTER_SERVER_ADDR=:8080 ./bin/server
```

Sanity check (from any machine):

```
curl http://192.168.1.10:8080/healthz
```

3) Start Peer A on laptop A (e.g., 192.168.1.11)

```
mkdir -p /home/user/peerA
./bin/client -cmd serve -server http://192.168.1.10:8080 -dir /home/user/peerA -addr http://192.168.1.11:9000 -bind :9000
```

4) Start Peer B on laptop B (e.g., 192.168.1.12)

```
mkdir -p /home/user/peerB
./bin/client -cmd serve -server http://192.168.1.10:8080 -dir /home/user/peerB -addr http://192.168.1.12:9001 -bind :9001
```

5) Put a file on Peer A and verify listing from another machine

```
echo "hello" > /home/user/peerA/hello.txt
curl http://192.168.1.11:9000/files
curl http://192.168.1.11:9000/files/hello.txt
```

6) Verify search via server

```
./bin/client -cmd search -server http://192.168.1.10:8080 -q hello
```

You should see the file listed with Peer A's address. With replication factor 2, the server will issue a task to Peer B; within ~15s you should see logs on Peer B pulling hello.txt from Peer A.

7) Verify the file exists on Peer B after replication

```
ls -l /home/user/peerB/hello.txt
curl http://192.168.1.12:9001/files
curl http://192.168.1.12:9001/files/hello.txt
```

Troubleshooting tips
- Use the LAN IPs (not 127.0.0.1) for -addr and -server.
- Ensure -bind uses a host/port that listens on the right interfaces (":9000" or "0.0.0.0:9000").
- Check server health: `curl http://<server-ip>:8080/healthz`.
- Query the server for known peers/files: `curl "http://<server-ip>:8080/search?q=hello"`.
- If you see connection refused from a peer, verify the peer process is running and the OS allows inbound on that port.

## API overview (for reference)

- Server endpoints (JSON):
	- POST /register { peer, files[] } -> { ok, tasks[] }
	- POST /heartbeat { peerId } -> { ok, tasks[] }
	- GET  /search?q=substr -> { matches: [ { file, peers[] } ] }
	- GET  /peers?file=name -> { file, peers[] }
	- POST /announce { peer, file }

- Peer endpoints:
	- GET /files -> [ { name, size } ]
	- GET /files/{name} -> file content

## Notes and limitations

- State is in-memory only. Restarting the server drops knowledge of registered peers and files until peers register again.
- Filenames are used as identifiers. Hashing and content verification hooks are present but not enforced in this baseline.
- Replication planner is simple: it selects the lexicographically smallest host as the source and assigns targets in peer ID order.
- Security is minimal. Do not expose this outside trusted networks.

## Project structure

```
server/           # Central index server (main)
client/           # Peer client and mini HTTP file server (main)
pkg/shared/       # Shared JSON types used by both binaries
```

## System description (concise)

- Architecture: Central tracker (Napster-style) + peer HTTP file servers
- Discovery: Server returns list of peers hosting requested files
- Replication: Server issues pull tasks to maintain N copies
- Liveness: Heartbeats and TTL-based pruning
