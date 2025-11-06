package shared

import "time"

// Peer represents a client node in the network.
type Peer struct {
	ID       string    `json:"id"`
	Addr     string    `json:"addr"` // http://host:port where this peer serves files
	LastSeen time.Time `json:"lastSeen"`
}

// FileInfo represents a file shared by a peer.
type FileInfo struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Hash string `json:"hash"` // optional content hash; client may leave empty
}

// RegisterRequest is used by a peer to register/update its presence and file list.
type RegisterRequest struct {
	Peer  Peer       `json:"peer"`
	Files []FileInfo `json:"files"`
}

// ReplicationTask instructs a peer to fetch a file from another peer.
type ReplicationTask struct {
	File       FileInfo `json:"file"`
	SourcePeer Peer     `json:"sourcePeer"`
}

// RegisterResponse may include replication tasks for the registering peer.
type RegisterResponse struct {
	OK    bool              `json:"ok"`
	Tasks []ReplicationTask `json:"tasks"`
}

// HeartbeatRequest/Response keep a peer alive and optionally deliver tasks.
type HeartbeatRequest struct {
	PeerID string `json:"peerId"`
}

type HeartbeatResponse struct {
	OK    bool              `json:"ok"`
	Tasks []ReplicationTask `json:"tasks"`
}

// SearchResponse returns matching files and the peers hosting them.
type SearchResponse struct {
	Matches []SearchMatch `json:"matches"`
}

type SearchMatch struct {
	File  FileInfo `json:"file"`
	Peers []Peer   `json:"peers"`
}

// AnnounceRequest indicates a peer has obtained a file (e.g., after download/replication).
type AnnounceRequest struct {
	Peer Peer     `json:"peer"`
	File FileInfo `json:"file"`
}
