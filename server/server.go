package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	shared "github.com/vivekhruday05/Napster-With-Replication/pkg/shared"
)

type fileEntry struct {
	Info  shared.FileInfo
	Peers map[string]shared.Peer // peerID -> Peer
}

type Server struct {
	mu                sync.Mutex
	replicationFactor int
	peerTTL           time.Duration

	peers     map[string]shared.Peer              // peerID -> Peer
	peerFiles map[string]map[string]struct{}      // peerID -> set(filename)
	files     map[string]*fileEntry               // filename -> entry
	queue     map[string][]shared.ReplicationTask // peerID -> tasks
}

func NewServer() *Server {
	replica := 2
	if v := os.Getenv("NAPSTER_REPLICATION_FACTOR"); v != "" {
		if n, err := strconvAtoiSafe(v); err == nil && n > 0 {
			replica = n
		}
	}
	s := &Server{
		replicationFactor: replica,
		peerTTL:           45 * time.Second,
		peers:             make(map[string]shared.Peer),
		peerFiles:         make(map[string]map[string]struct{}),
		files:             make(map[string]*fileEntry),
		queue:             make(map[string][]shared.ReplicationTask),
	}
	go s.cleanupLoop()
	return s
}

func strconvAtoiSafe(s string) (int, error) {
	var n int
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, fmtError("non-numeric")
		}
		n = n*10 + int(ch-'0')
	}
	return n, nil
}

// minimal fmt error without importing fmt just for small binary
func fmtError(msg string) error { return &simpleError{msg} }

type simpleError struct{ s string }

func (e *simpleError) Error() string { return e.s }

func (s *Server) cleanupLoop() {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for range t.C {
		s.pruneStale()
	}
}

func (s *Server) pruneStale() {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, p := range s.peers {
		if now.Sub(p.LastSeen) > s.peerTTL {
			log.Printf("peer %s stale; removing", id)
			// remove from file indexes
			if files, ok := s.peerFiles[id]; ok {
				for fname := range files {
					if fe, ok2 := s.files[fname]; ok2 {
						delete(fe.Peers, id)
						if len(fe.Peers) == 0 {
							delete(s.files, fname)
						}
					}
				}
			}
			delete(s.peerFiles, id)
			delete(s.peers, id)
			delete(s.queue, id)
		}
	}
}

func registerRoutes(mux *http.ServeMux, srv *Server) {
	mux.HandleFunc("/register", srv.handleRegister)
	mux.HandleFunc("/heartbeat", srv.handleHeartbeat)
	mux.HandleFunc("/search", srv.handleSearch)
	mux.HandleFunc("/peers", srv.handlePeers)
	mux.HandleFunc("/announce", srv.handleAnnounce)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200); w.Write([]byte("ok")) })
}

func decodeJSON[T any](w http.ResponseWriter, r *http.Request, dst *T) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req shared.RegisterRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	p := req.Peer
	p.LastSeen = now
	s.peers[p.ID] = p
	if _, ok := s.peerFiles[p.ID]; !ok {
		s.peerFiles[p.ID] = make(map[string]struct{})
	}
	// update files
	for _, f := range req.Files {
		fe := s.files[f.Name]
		if fe == nil {
			fe = &fileEntry{Info: f, Peers: make(map[string]shared.Peer)}
			s.files[f.Name] = fe
		}
		fe.Info = f // latest metadata
		fe.Peers[p.ID] = p
		s.peerFiles[p.ID][f.Name] = struct{}{}
	}
	// plan replication
	s.planReplicationLocked()

	// return any queued tasks
	tasks := s.queue[p.ID]
	s.queue[p.ID] = nil
	s.mu.Unlock() // unlock while encoding
	json.NewEncoder(w).Encode(shared.RegisterResponse{OK: true, Tasks: tasks})
	s.mu.Lock() // re-lock to satisfy defer (no-op if we had no panic)
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req shared.HeartbeatRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	s.mu.Lock()
	p, ok := s.peers[req.PeerID]
	if ok {
		p.LastSeen = time.Now()
		s.peers[req.PeerID] = p
	}
	tasks := s.queue[req.PeerID]
	s.queue[req.PeerID] = nil
	s.mu.Unlock()
	json.NewEncoder(w).Encode(shared.HeartbeatResponse{OK: true, Tasks: tasks})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if q == "" {
		http.Error(w, "missing q", 400)
		return
	}
	res := shared.SearchResponse{}

	s.mu.Lock()
	for name, fe := range s.files {
		if strings.Contains(strings.ToLower(name), q) {
			match := shared.SearchMatch{File: fe.Info}
			for _, p := range fe.Peers {
				match.Peers = append(match.Peers, p)
			}
			// stable order
			sort.Slice(match.Peers, func(i, j int) bool { return match.Peers[i].ID < match.Peers[j].ID })
			res.Matches = append(res.Matches, match)
		}
	}
	// stable order by filename
	sort.Slice(res.Matches, func(i, j int) bool { return res.Matches[i].File.Name < res.Matches[j].File.Name })
	s.mu.Unlock()

	json.NewEncoder(w).Encode(res)
}

func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	name := r.URL.Query().Get("file")
	if name == "" {
		http.Error(w, "missing file", 400)
		return
	}
	var peers []shared.Peer
	var info shared.FileInfo
	ok := false

	s.mu.Lock()
	if fe, exists := s.files[name]; exists {
		ok = true
		info = fe.Info
		for _, p := range fe.Peers {
			peers = append(peers, p)
		}
		sort.Slice(peers, func(i, j int) bool { return peers[i].ID < peers[j].ID })
	}
	s.mu.Unlock()
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	json.NewEncoder(w).Encode(shared.SearchMatch{File: info, Peers: peers})
}

func (s *Server) handleAnnounce(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req shared.AnnounceRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	s.mu.Lock()
	p := req.Peer
	if old, ok := s.peers[p.ID]; ok {
		p.LastSeen = old.LastSeen
	}
	s.peers[p.ID] = p
	if _, ok := s.peerFiles[p.ID]; !ok {
		s.peerFiles[p.ID] = make(map[string]struct{})
	}
	fe := s.files[req.File.Name]
	if fe == nil {
		fe = &fileEntry{Info: req.File, Peers: make(map[string]shared.Peer)}
		s.files[req.File.Name] = fe
	}
	fe.Info = req.File
	fe.Peers[p.ID] = p
	s.peerFiles[p.ID][req.File.Name] = struct{}{}
	// re-plan
	s.planReplicationLocked()
	s.mu.Unlock()
	w.WriteHeader(204)
}

func (s *Server) planReplicationLocked() {
	// Basic algorithm: for each file with copies < replicationFactor, enqueue tasks for randomly chosen peers who don't have the file to pull from an existing peer.
	// Deterministic: choose lexicographically smallest source peer for stability; choose target peers by ID order.
	for _, fe := range s.files {
		needed := s.replicationFactor - len(fe.Peers)
		if needed <= 0 || len(fe.Peers) == 0 {
			continue
		}
		// build list of candidate targets
		var targets []shared.Peer
		for id, p := range s.peers {
			if _, has := fe.Peers[id]; !has {
				targets = append(targets, p)
			}
		}
		if len(targets) == 0 {
			continue
		}
		sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
		// choose source peer: lowest ID among hosts
		var hostIDs []string
		for id := range fe.Peers {
			hostIDs = append(hostIDs, id)
		}
		sort.Strings(hostIDs)
		src := fe.Peers[hostIDs[0]]
		for i := 0; i < needed && i < len(targets); i++ {
			peer := targets[i]
			// enqueue task for target peer
			s.queue[peer.ID] = append(s.queue[peer.ID], shared.ReplicationTask{File: fe.Info, SourcePeer: src})
		}
	}
}
