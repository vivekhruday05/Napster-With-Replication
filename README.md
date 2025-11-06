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
./bin/client -cmd serve -server http://localhost:8080 -dir /tmp/peer1 -addr http://localhost:9000 &
./bin/client -cmd serve -server http://localhost:8080 -dir /tmp/peer2 -addr http://localhost:9001 &
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

## License

MIT-like for educational use. Replace as needed.