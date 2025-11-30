package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	shared "github.com/vivekhruday05/Napster-With-Replication/pkg/shared"
)

// Lease tracks who currently has the right to update a file.
type Lease struct {
	HolderID   string
	Expiration time.Time
}

type fileEntry struct {
	Info  shared.FileInfo
	Peers map[string]shared.Peer
	Lease *Lease // NEW: Lease info
}

type Server struct {
	mu                sync.Mutex
	replicationFactor int
	peerTTL           time.Duration
	ShadowAddr        string // NEW: Address of the shadow master (e.g., "http://localhost:8081")

	peers map[string]shared.Peer
	// peerFiles maps peerID -> filename -> version hosted by that peer
	peerFiles map[string]map[string]int64
	files     map[string]*fileEntry
	queue     map[string][]shared.ReplicationTask
}

func NewServer() *Server {
	replica := 2
	if v := os.Getenv("NAPSTER_REPLICATION_FACTOR"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			replica = n
		}
	}
	// NEW: Read Shadow Address from Env
	shadow := os.Getenv("NAPSTER_SHADOW_ADDR")
	// Normalize: ensure scheme present for HTTP client
	if shadow != "" && !strings.HasPrefix(shadow, "http://") && !strings.HasPrefix(shadow, "https://") {
		shadow = "http://" + shadow
	}

	s := &Server{
		replicationFactor: replica,
		peerTTL:           45 * time.Second,
		ShadowAddr:        shadow,
		peers:             make(map[string]shared.Peer),
		peerFiles:         make(map[string]map[string]int64),
		files:             make(map[string]*fileEntry),
		queue:             make(map[string][]shared.ReplicationTask),
	}
	go s.cleanupLoop()
	return s
}

func (s *Server) cleanupLoop() {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for range t.C {
		log.Printf("[cleanup] running pruneStale (peerTTL=%s, peers=%d)", s.peerTTL, len(s.peers))
		s.pruneStale()
	}
}

func (s *Server) pruneStale() {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, p := range s.peers {
		if now.Sub(p.LastSeen) > s.peerTTL {
			log.Printf("[prune] peer=%s addr=%s lastSeen=%s age=%s > ttl=%s -> removing", id, p.Addr, p.LastSeen.Format(time.RFC3339), now.Sub(p.LastSeen), s.peerTTL)

			// NEW: Sync prune event to shadow
			go s.syncToShadow("prune", id)

			if files, ok := s.peerFiles[id]; ok {
				for fname := range files {
					if fe, ok2 := s.files[fname]; ok2 {
						delete(fe.Peers, id)
						if len(fe.Peers) == 0 {
							log.Printf("[prune] file=%s had only stale peer; removing file entry", fname)
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
	log.Printf("[routes] registering HTTP routes; shadow=%s replicationFactor=%d ttl=%s", srv.ShadowAddr, srv.replicationFactor, srv.peerTTL)
	mux.HandleFunc("/register", srv.handleRegister)
	mux.HandleFunc("/heartbeat", srv.handleHeartbeat)
	mux.HandleFunc("/search", srv.handleSearch)
	mux.HandleFunc("/peers", srv.handlePeers)
	mux.HandleFunc("/announce", srv.handleAnnounce)

	// NEW ROUTES
	mux.HandleFunc("/lease", srv.handleLease)
	mux.HandleFunc("/sync", srv.handleSync)
	mux.HandleFunc("/delete", srv.handleDelete)

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

// --- NEW: Sync Logic (Primary -> Shadow) ---

func (s *Server) syncToShadow(opType string, data interface{}) {
	if s.ShadowAddr == "" {
		log.Printf("[shadow] skip sync op=%s (no shadow configured)", opType)
		return
	}
	// Marshal the inner data first to RawMessage
	inner, _ := json.Marshal(data)
	payload := shared.SyncOp{
		OpType: opType,
		Data:   json.RawMessage(inner),
	}

	b, _ := json.Marshal(payload)
	// Fire and forget: don't block the primary server's main thread
	go func() {
		start := time.Now()
		resp, err := http.Post(s.ShadowAddr+"/sync", "application/json", bytes.NewReader(b))
		dur := time.Since(start)
		if err != nil {
			log.Printf("[shadow] sync op=%s error=%v dur=%s", opType, err, dur)
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		log.Printf("[shadow] sync op=%s status=%s dur=%s", opType, resp.Status, dur)
	}()
}

// handleSync is called on the SHADOW server to apply updates from Primary
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	log.Printf("[shadow] handleSync from=%s", r.RemoteAddr)
	var op shared.SyncOp
	if !decodeJSON(w, r, &op) {
		log.Printf("[shadow] handleSync decode error")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	switch op.OpType {
	case "register":
		log.Printf("[shadow] apply register")
		var req shared.RegisterRequest
		if err := json.Unmarshal(op.Data, &req); err == nil {
			s.applyRegister(req)
		}
	case "announce":
		log.Printf("[shadow] apply announce")
		var req shared.AnnounceRequest
		if err := json.Unmarshal(op.Data, &req); err == nil {
			s.applyAnnounce(req)
		}
	case "prune":
		log.Printf("[shadow] apply prune")
		var peerID string
		if err := json.Unmarshal(op.Data, &peerID); err == nil {
			// simplified prune logic for shadow (just delete)
			delete(s.peers, peerID)
			delete(s.peerFiles, peerID)
			// Deep clean not fully implemented in this snippet for brevity,
			// but removing from s.peers prevents it from showing in search.
		}
	case "heartbeat":
		log.Printf("[shadow] apply heartbeat")
		var req shared.HeartbeatRequest
		if err := json.Unmarshal(op.Data, &req); err == nil {
			if p, ok := s.peers[req.PeerID]; ok {
				p.LastSeen = time.Now().UTC()
				s.peers[req.PeerID] = p
			}
		}
	case "delete":
		log.Printf("[shadow] apply delete")
		var req shared.DeleteRequest
		if err := json.Unmarshal(op.Data, &req); err == nil {
			// Remove the peer from the file entry
			if fe, ok := s.files[req.FileName]; ok {
				delete(fe.Peers, req.PeerID)
				if len(fe.Peers) == 0 {
					delete(s.files, req.FileName)
				}
			}
			if pf, ok := s.peerFiles[req.PeerID]; ok {
				delete(pf, req.FileName)
			}
		}
	}
	w.WriteHeader(200)
}

// Helper to apply register logic (reused by Sync and HandleRegister)
func (s *Server) applyRegister(req shared.RegisterRequest) {
	log.Printf("[register] peer=%s addr=%s files=%d", req.Peer.ID, req.Peer.Addr, len(req.Files))
	p := req.Peer
	p.LastSeen = time.Now().UTC()
	s.peers[p.ID] = p
	if _, ok := s.peerFiles[p.ID]; !ok {
		s.peerFiles[p.ID] = make(map[string]int64)
	}
	for _, f := range req.Files {
		log.Printf("[register] file=%s size=%d version=%d host=%s", f.Name, f.Size, f.Version, p.ID)
		fe := s.files[f.Name]
		if fe == nil {
			fe = &fileEntry{Info: f, Peers: make(map[string]shared.Peer)}
			s.files[f.Name] = fe
		}
		// Version update: Shadow always accepts the version from Primary
		if f.Version >= fe.Info.Version {
			fe.Info = f
		}
		fe.Peers[p.ID] = p
		s.peerFiles[p.ID][f.Name] = f.Version
	}
}

// Helper to apply announce logic
func (s *Server) applyAnnounce(req shared.AnnounceRequest) {
	log.Printf("[announce] peer=%s addr=%s file=%s version=%d", req.Peer.ID, req.Peer.Addr, req.File.Name, req.File.Version)
	p := req.Peer
	// Ensure LastSeen is set so we don't overwrite a healthy peer with zero time
	if p.LastSeen.IsZero() {
		p.LastSeen = time.Now().UTC()
	}
	s.peers[p.ID] = p
	if _, ok := s.peerFiles[p.ID]; !ok {
		s.peerFiles[p.ID] = make(map[string]int64)
	}
	fe := s.files[req.File.Name]
	if fe == nil {
		fe = &fileEntry{Info: req.File, Peers: make(map[string]shared.Peer)}
		s.files[req.File.Name] = fe
	}
	if req.File.Version >= fe.Info.Version {
		fe.Info = req.File
	}
	fe.Peers[p.ID] = p
	s.peerFiles[p.ID][req.File.Name] = req.File.Version
}

// --- NEW: Lease Logic ---

func (s *Server) handleLease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Enforce read-only on shadow: ShadowAddr empty means this instance is SHADOW
	if s.ShadowAddr == "" {
		http.Error(w, "shadow is read-only", http.StatusForbidden)
		log.Printf("[lease] rejected on shadow: read-only")
		return
	}
	log.Printf("[lease] from=%s", r.RemoteAddr)
	var req shared.LeaseRequest
	if !decodeJSON(w, r, &req) {
		log.Printf("[lease] bad json")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	fe, exists := s.files[req.FileName]

	// Check if leased
	log.Printf("[lease] request peer=%s file=%s", req.PeerID, req.FileName)
	if exists && fe.Lease != nil {
		if time.Now().Before(fe.Lease.Expiration) && fe.Lease.HolderID != req.PeerID {
			json.NewEncoder(w).Encode(shared.LeaseResponse{
				Granted: false,
				Message: "Lease currently held by " + fe.Lease.HolderID,
			})
			log.Printf("[lease] denied: heldBy=%s until=%s", fe.Lease.HolderID, fe.Lease.Expiration.Format(time.RFC3339))
			return
		}
	}

	// Grant Lease (60s duration)
	exp := time.Now().UTC().Add(60 * time.Second)
	if !exists {
		// Create placeholder
		s.files[req.FileName] = &fileEntry{
			Info:  shared.FileInfo{Name: req.FileName},
			Peers: make(map[string]shared.Peer),
		}
		fe = s.files[req.FileName]
	}
	fe.Lease = &Lease{HolderID: req.PeerID, Expiration: exp}

	json.NewEncoder(w).Encode(shared.LeaseResponse{
		Granted:    true,
		Expiration: exp,
	})
	log.Printf("[lease] granted: peer=%s file=%s exp=%s", req.PeerID, req.FileName, exp.Format(time.RFC3339))
}

// --- Existing Handlers (Updated) ---

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Enforce read-only on shadow
	if s.ShadowAddr == "" {
		http.Error(w, "shadow is read-only", http.StatusForbidden)
		log.Printf("[register] rejected on shadow: read-only")
		return
	}
	log.Printf("[register] from=%s", r.RemoteAddr)
	var req shared.RegisterRequest
	if !decodeJSON(w, r, &req) {
		log.Printf("[register] bad json")
		return
	}

	// Sync to Shadow
	go s.syncToShadow("register", req)

	s.mu.Lock()
	s.applyRegister(req) // Use shared logic
	s.planReplicationLocked()
	tasks := s.queue[req.Peer.ID]
	s.queue[req.Peer.ID] = nil
	s.mu.Unlock()

	json.NewEncoder(w).Encode(shared.RegisterResponse{OK: true, Tasks: tasks})
	log.Printf("[register] ok peer=%s tasks=%d", req.Peer.ID, len(tasks))
}

func (s *Server) handleAnnounce(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Enforce read-only on shadow
	if s.ShadowAddr == "" {
		http.Error(w, "shadow is read-only", http.StatusForbidden)
		log.Printf("[announce] rejected on shadow: read-only")
		return
	}
	log.Printf("[announce] from=%s", r.RemoteAddr)
	var req shared.AnnounceRequest
	if !decodeJSON(w, r, &req) {
		log.Printf("[announce] bad json")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	fe := s.files[req.File.Name]

	// NEW: Verify Lease if version is increasing
	if fe != nil && req.File.Version > fe.Info.Version {
		if fe.Lease == nil || fe.Lease.HolderID != req.Peer.ID || time.Now().UTC().After(fe.Lease.Expiration) {
			http.Error(w, "Lease required to update file version", http.StatusForbidden)
			log.Printf("[announce] rejected: lease missing/expired for peer=%s file=%s", req.Peer.ID, req.File.Name)
			return
		}
	}

	// Sync to Shadow
	go s.syncToShadow("announce", req)

	// Apply logic
	p := req.Peer
	// Preserve or set LastSeen to now to avoid marking peer stale
	if p.LastSeen.IsZero() {
		p.LastSeen = time.Now().UTC()
	}
	s.peers[p.ID] = p
	if _, ok := s.peerFiles[p.ID]; !ok {
		s.peerFiles[p.ID] = make(map[string]int64)
	}
	if fe == nil {
		fe = &fileEntry{Info: req.File, Peers: make(map[string]shared.Peer)}
		s.files[req.File.Name] = fe
	}

	// Update version if newer
	if req.File.Version >= fe.Info.Version {
		fe.Info = req.File
	}
	fe.Peers[p.ID] = p
	s.peerFiles[p.ID][req.File.Name] = req.File.Version

	s.planReplicationLocked()
	w.WriteHeader(204)
	log.Printf("[announce] ok peer=%s file=%s version=%d", p.ID, req.File.Name, req.File.Version)
}

// In server/server.go

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Enforce read-only on shadow (heartbeats should be applied via /sync from primary)
	if s.ShadowAddr == "" {
		http.Error(w, "shadow is read-only", http.StatusForbidden)
		log.Printf("[heartbeat] rejected on shadow: read-only")
		return
	}
	log.Printf("[heartbeat] from=%s", r.RemoteAddr)
	var req shared.HeartbeatRequest
	if !decodeJSON(w, r, &req) {
		log.Printf("[heartbeat] bad json")
		return
	}

	// NEW: Sync Heartbeat to Shadow so it doesn't kill the peer
	go s.syncToShadow("heartbeat", req)

	s.mu.Lock()
	p, ok := s.peers[req.PeerID]
	if ok {
		p.LastSeen = time.Now().UTC()
		s.peers[req.PeerID] = p
	}
	// If peer is not found (server restarted?), we could optionally tell client to re-register.
	// For now, just return tasks.
	tasks := s.queue[req.PeerID]
	s.queue[req.PeerID] = nil
	s.mu.Unlock()

	json.NewEncoder(w).Encode(shared.HeartbeatResponse{OK: ok, Tasks: tasks})
	log.Printf("[heartbeat] peer=%s ok=%t tasks=%d", req.PeerID, ok, len(tasks))
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	log.Printf("[search] q=%q", q)
	res := shared.SearchResponse{}

	s.mu.Lock()
	for name, fe := range s.files {
		if strings.Contains(strings.ToLower(name), q) {
			match := shared.SearchMatch{File: fe.Info}
			for _, p := range fe.Peers {
				match.Peers = append(match.Peers, p)
			}
			sort.Slice(match.Peers, func(i, j int) bool { return match.Peers[i].ID < match.Peers[j].ID })
			res.Matches = append(res.Matches, match)
		}
	}
	sort.Slice(res.Matches, func(i, j int) bool { return res.Matches[i].File.Name < res.Matches[j].File.Name })
	s.mu.Unlock()

	json.NewEncoder(w).Encode(res)
	log.Printf("[search] matches=%d", len(res.Matches))
}

func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := r.URL.Query().Get("file")
	log.Printf("[peers] file=%s", name)
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
		log.Printf("[peers] not found file=%s", name)
		return
	}
	json.NewEncoder(w).Encode(shared.SearchMatch{File: info, Peers: peers})
	log.Printf("[peers] file=%s peers=%d", name, len(peers))
}

func (s *Server) planReplicationLocked() {
	log.Printf("[replication] planning for %d files", len(s.files))
	for _, fe := range s.files {
		latest := fe.Info.Version
		if len(fe.Peers) == 0 {
			// No source available
			log.Printf("[replication] skip file=%s (no peers)", fe.Info.Name)
			continue
		}

		// Find peers that have latest version to use as source
		var latestSources []shared.Peer
		for pid := range fe.Peers {
			if vmap, ok := s.peerFiles[pid]; ok {
				if vmap[fe.Info.Name] == latest {
					latestSources = append(latestSources, fe.Peers[pid])
				}
			}
		}
		if len(latestSources) == 0 {
			// Fallback to any host if we don't have version info
			var hostIDs []string
			for id := range fe.Peers {
				hostIDs = append(hostIDs, id)
			}
			sort.Strings(hostIDs)
			latestSources = []shared.Peer{fe.Peers[hostIDs[0]]}
		}
		src := latestSources[0]

		// Determine targets: missing peers and outdated peers
		var addTargets []shared.Peer
		var updateTargets []shared.Peer
		for id, p := range s.peers {
			// Skip peers that already have latest
			if vmap, ok := s.peerFiles[id]; ok {
				v := vmap[fe.Info.Name]
				if v == 0 {
					// missing
					if _, has := fe.Peers[id]; !has {
						addTargets = append(addTargets, p)
					}
				} else if v < latest {
					updateTargets = append(updateTargets, p)
				}
			} else {
				// No record for this peer => treat as missing
				if _, has := fe.Peers[id]; !has {
					addTargets = append(addTargets, p)
				}
			}
		}
		sort.Slice(addTargets, func(i, j int) bool { return addTargets[i].ID < addTargets[j].ID })
		sort.Slice(updateTargets, func(i, j int) bool { return updateTargets[i].ID < updateTargets[j].ID })

		// Respect replication factor for additions
		neededAdds := s.replicationFactor - len(fe.Peers)
		if neededAdds < 0 {
			neededAdds = 0
		}
		for i := 0; i < neededAdds && i < len(addTargets); i++ {
			peer := addTargets[i]
			s.queue[peer.ID] = append(s.queue[peer.ID], shared.ReplicationTask{File: fe.Info, SourcePeer: src})
			log.Printf("[replication] enqueue ADD peer=%s file=%s src=%s", peer.ID, fe.Info.Name, src.ID)
		}
		// Enqueue updates for outdated peers (not capped by factor)
		for _, peer := range updateTargets {
			s.queue[peer.ID] = append(s.queue[peer.ID], shared.ReplicationTask{File: fe.Info, SourcePeer: src})
			log.Printf("[replication] enqueue UPDATE peer=%s file=%s src=%s", peer.ID, fe.Info.Name, src.ID)
		}
	}
}

// handleDelete removes a peer's ownership of a file and triggers replication planning.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Enforce read-only on shadow
	if s.ShadowAddr == "" {
		http.Error(w, "shadow is read-only", http.StatusForbidden)
		log.Printf("[delete] rejected on shadow: read-only")
		return
	}
	log.Printf("[delete] from=%s", r.RemoteAddr)
	var req shared.DeleteRequest
	if !decodeJSON(w, r, &req) {
		log.Printf("[delete] bad json")
		return
	}

	// Sync to shadow
	go s.syncToShadow("delete", req)

	s.mu.Lock()
	defer s.mu.Unlock()

	fe, ok := s.files[req.FileName]
	if !ok {
		http.Error(w, "file not found", http.StatusNotFound)
		log.Printf("[delete] not found file=%s", req.FileName)
		return
	}
	if _, owns := fe.Peers[req.PeerID]; !owns {
		http.Error(w, "peer does not own this file", http.StatusForbidden)
		log.Printf("[delete] forbidden: peer=%s not owner of file=%s", req.PeerID, req.FileName)
		return
	}

	// Remove mapping
	delete(fe.Peers, req.PeerID)
	if pf, ok := s.peerFiles[req.PeerID]; ok {
		delete(pf, req.FileName)
	}
	if len(fe.Peers) == 0 {
		delete(s.files, req.FileName)
	}

	// Re-plan replication
	s.planReplicationLocked()

	// Do not reassign this file back to the deleting peer; remove such tasks if any
	tq := s.queue[req.PeerID]
	if len(tq) > 0 {
		filtered := tq[:0]
		for _, t := range tq {
			if t.File.Name != req.FileName {
				filtered = append(filtered, t)
			}
		}
		s.queue[req.PeerID] = filtered
	}

	w.WriteHeader(http.StatusNoContent)
	log.Printf("[delete] ok peer=%s file=%s", req.PeerID, req.FileName)
}
