package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	shared "github.com/vivekhruday05/Napster-With-Replication/pkg/shared"
)

type Client struct {
	Servers    []string // UPDATED: List of servers (Primary, Shadow)
	SharedDir  string
	PeerAddr   string
	BindAddr   string
	peerID     string
}

// EnsureDir creates the shared folder
func (c *Client) EnsureDir() error {
	return os.MkdirAll(c.SharedDir, 0o755)
}

func (c *Client) peerIDFromAddr() string {
	u, err := url.Parse(c.PeerAddr)
	if err != nil || u.Host == "" {
		return "peer-unknown"
	}
	return "peer-" + strings.ReplaceAll(u.Host, ":", "-")
}

// doRequest iterates through the server list until one succeeds
func (c *Client) doRequest(method, path string, body any, out any) error {
	var lastErr error
	for _, base := range c.Servers {
		var reqBody io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			reqBody = bytes.NewReader(b)
		}

		req, err := http.NewRequest(method, base+path, reqBody)
		if err != nil {
			return err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Printf("Failed to contact %s: %v. Trying next...", base, err)
			lastErr = err
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 300 {
			// If server returns error like 404/403, we generally shouldn't try the shadow 
			// because it implies the server IS working but rejected us.
			// But for connection errors, we continue.
			b, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("%s returned %s: %s", base, resp.Status, string(b))
		}

		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				return err
			}
		}
		return nil // Success
	}
	if lastErr != nil {
		return fmt.Errorf("all servers failed: %v", lastErr)
	}
	return errors.New("no servers configured")
}

// Serve starts the peer server and background loop
func (c *Client) Serve() error {
	if c.peerID == "" {
		c.peerID = c.peerIDFromAddr()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/files", c.handleListFiles)
	mux.HandleFunc("/files/", c.handleGetFile)

	stop := make(chan struct{})
	go c.backgroundLoop(stop)
	defer close(stop)

	addr := c.BindAddr
	if strings.TrimSpace(addr) == "" {
		addr = ":9000"
	}
	log.Printf("Peer %s serving files from %s on %s", c.peerID, c.SharedDir, addr)
	return http.ListenAndServe(addr, mux)
}

func (c *Client) backgroundLoop(stop <-chan struct{}) {
	for {
		if err := c.registerNow(); err != nil {
			log.Printf("register error: %v", err)
		}
		select {
		case <-time.After(15 * time.Second):
		case <-stop:
			return
		}
		if err := c.heartbeatNow(); err != nil {
			log.Printf("heartbeat error: %v", err)
		}
	}
}

func (c *Client) scanFiles() ([]shared.FileInfo, error) {
	entries, err := os.ReadDir(c.SharedDir)
	if err != nil {
		return nil, err
	}
	var files []shared.FileInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		fi, err := os.Stat(filepath.Join(c.SharedDir, name))
		if err != nil {
			continue
		}
		// For the project, simple scan. 
		// Note: Versions are managed via 'update' command mostly.
		files = append(files, shared.FileInfo{Name: name, Size: fi.Size()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, nil
}

func (c *Client) registerNow() error {
	files, err := c.scanFiles()
	if err != nil {
		return err
	}
	peer := shared.Peer{ID: c.peerIDFromAddr(), Addr: c.PeerAddr}
	req := shared.RegisterRequest{Peer: peer, Files: files}
	var resp shared.RegisterResponse
	
	// Use doRequest (Failover)
	if err := c.doRequest("POST", "/register", req, &resp); err != nil {
		return err
	}
	c.processTasks(resp.Tasks)
	return nil
}

func (c *Client) heartbeatNow() error {
	req := shared.HeartbeatRequest{PeerID: c.peerIDFromAddr()}
	var resp shared.HeartbeatResponse
	if err := c.doRequest("POST", "/heartbeat", req, &resp); err != nil {
		return err
	}
	c.processTasks(resp.Tasks)
	return nil
}

func (c *Client) processTasks(tasks []shared.ReplicationTask) {
	for _, t := range tasks {
		log.Printf("replication task: pull %s from %s", t.File.Name, t.SourcePeer.Addr)
		if err := c.pullFile(t.SourcePeer.Addr, t.File.Name); err != nil {
			log.Printf("replication failed for %s: %v", t.File.Name, err)
			continue
		}
		// Announce: Note, replication copies the version too in a real system
		_ = c.doRequest("POST", "/announce", shared.AnnounceRequest{Peer: shared.Peer{ID: c.peerIDFromAddr(), Addr: c.PeerAddr}, File: t.File}, nil)
	}
}

// --- NEW: Update File Workflow (Lease -> Edit -> Announce) ---

func (c *Client) UpdateFile(filename string) error {
	// 1. Acquire Lease
	log.Printf("Requesting lease for %s...", filename)
	leaseReq := shared.LeaseRequest{PeerID: c.peerIDFromAddr(), FileName: filename}
	var leaseResp shared.LeaseResponse
	if err := c.doRequest("POST", "/lease", leaseReq, &leaseResp); err != nil {
		return err
	}
	if !leaseResp.Granted {
		return fmt.Errorf("lease denied: %s", leaseResp.Message)
	}
	log.Printf("Lease acquired! Valid until %v", leaseResp.Expiration)

	// 2. Simulate "Edit" by just reading current stats and incrementing version
	path := filepath.Join(c.SharedDir, filename)
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("file not found locally: %v", err)
	}

	// In a real app, you'd fetch the current version from server first.
	// Here we just use a timestamp based version for monotonicity
	newVersion := time.Now().UnixNano()

	updatedInfo := shared.FileInfo{
		Name:    filename,
		Size:    fi.Size(),
		Version: newVersion,
	}

	// 3. Announce Update
	req := shared.AnnounceRequest{
		Peer: shared.Peer{ID: c.peerIDFromAddr(), Addr: c.PeerAddr},
		File: updatedInfo,
	}
	if err := c.doRequest("POST", "/announce", req, nil); err != nil {
		return err
	}
	log.Printf("File %s updated to version %d", filename, newVersion)
	return nil
}

// --- Standard Handlers ---

func (c *Client) handleListFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	files, err := c.scanFiles()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	json.NewEncoder(w).Encode(files)
}

func (c *Client) handleGetFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/files/")
	if name == "" {
		http.Error(w, "missing name", 400)
		return
	}
	path := filepath.Join(c.SharedDir, filepath.Clean(name))
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "not found", 404)
		return
	}
	defer f.Close()
	http.ServeContent(w, r, name, time.Now(), f)
}

func (c *Client) Search(query string) error {
	path := "/search?q=" + url.QueryEscape(query)
	var sr shared.SearchResponse
	if err := c.doRequest("GET", path, nil, &sr); err != nil {
		return err
	}
	if len(sr.Matches) == 0 {
		log.Printf("no matches for %q", query)
		return nil
	}
	for _, m := range sr.Matches {
		var hosts []string
		for _, p := range m.Peers {
			hosts = append(hosts, p.Addr)
		}
		log.Printf("%s (v%d, %d bytes) => %s", m.File.Name, m.File.Version, m.File.Size, strings.Join(hosts, ", "))
	}
	return nil
}

func (c *Client) Get(name string) error {
	// First, find peers
	var match shared.SearchMatch
	path := "/peers?file=" + url.QueryEscape(name)
	if err := c.doRequest("GET", path, nil, &match); err != nil {
		return err
	}

	if len(match.Peers) == 0 {
		return errors.New("no peers hosting that file")
	}
	// Pick first peer
	src := match.Peers[0]
	if err := c.pullFile(src.Addr, name); err != nil {
		return err
	}
	// Announce ownership
	return c.doRequest("POST", "/announce", shared.AnnounceRequest{Peer: shared.Peer{ID: c.peerIDFromAddr(), Addr: c.PeerAddr}, File: match.File}, nil)
}

func (c *Client) pullFile(peerBase, name string) error {
	u := strings.TrimRight(peerBase, "/") + "/files/" + url.PathEscape(name)
	resp, err := http.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed: %s", resp.Status)
	}
	path := filepath.Join(c.SharedDir, filepath.Clean(name))
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func (c *Client) ListLocal() error {
	files, err := c.scanFiles()
	if err != nil {
		return err
	}
	for _, f := range files {
		log.Printf("%s (%d bytes)", f.Name, f.Size)
	}
	return nil
}