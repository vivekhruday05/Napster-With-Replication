package shared

import (
	"encoding/json"
	"time"
)

// Peer represents a client node in the network.
type Peer struct {
	ID       string    `json:"id"`
	Addr     string    `json:"addr"` // http://host:port where this peer serves files
	LastSeen time.Time `json:"lastSeen"`
}

// FileInfo represents a file shared by a peer.
type FileInfo struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Hash    string `json:"hash"`
	Version int64  `json:"version"` // NEW: Version number for consistency
}

// --- NEW: Structures for Lease Management (Write Locking) ---

type LeaseRequest struct {
	PeerID   string `json:"peerId"`
	FileName string `json:"fileName"`
}

type LeaseResponse struct {
	Granted    bool      `json:"granted"`
	Expiration time.Time `json:"expiration"`
	Message    string    `json:"message"`
}

// --- NEW: Structures for Shadow Master Replication ---

// SyncOp is sent from Primary -> Shadow to keep state consistent.
type SyncOp struct {
	OpType string          `json:"opType"` // "register", "announce", "prune"
	Data   json.RawMessage `json:"data"`   // Raw JSON to be decoded based on OpType
}

// --- Existing Structures ---

type RegisterRequest struct {
	Peer  Peer       `json:"peer"`
	Files []FileInfo `json:"files"`
}

type ReplicationTask struct {
	File       FileInfo `json:"file"`
	SourcePeer Peer     `json:"sourcePeer"`
}

type RegisterResponse struct {
	OK    bool              `json:"ok"`
	Tasks []ReplicationTask `json:"tasks"`
}

type HeartbeatRequest struct {
	PeerID string `json:"peerId"`
}

type HeartbeatResponse struct {
	OK    bool              `json:"ok"`
	Tasks []ReplicationTask `json:"tasks"`
}

type SearchResponse struct {
	Matches []SearchMatch `json:"matches"`
}

type SearchMatch struct {
	File  FileInfo `json:"file"`
	Peers []Peer   `json:"peers"`
}

type AnnounceRequest struct {
	Peer Peer     `json:"peer"`
	File FileInfo `json:"file"`
}

// DeleteRequest is sent by a peer to indicate it is deleting a local file and
// should be removed as a host for that file on the server. The server will
// update its index and plan replication tasks if needed.
type DeleteRequest struct {
	PeerID   string `json:"peerId"`
	FileName string `json:"fileName"`
}

type DeleteResponse struct {
	OK bool `json:"ok"`
}
