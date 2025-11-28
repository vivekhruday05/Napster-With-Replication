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
	Servers   []string // UPDATED: List of servers (Primary, Shadow)
	SharedDir string
	PeerAddr  string
	BindAddr  string
	peerID    string
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
		start := time.Now()
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
			log.Printf("[client] request %s %s base=%s error=%v dur=%s", method, path, base, err, time.Since(start))
			lastErr = err
			continue
		}
		defer resp.Body.Close()

		// Failover logic:
		// - Don't retry on 4xx client errors (e.g., 403 Forbidden, 404 Not Found)
		//   as these are intentional rejections from the primary server.
		// - Retry on 5xx server errors or network issues, as the shadow may be healthy.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			b, _ := io.ReadAll(resp.Body)
			log.Printf("[client] request %s %s base=%s status=%s clientError body=%s", method, path, base, resp.Status, string(b))
			return fmt.Errorf("%s returned client error %s: %s", base, resp.Status, string(b))
		}
		if resp.StatusCode >= 500 {
			log.Printf("[client] request %s %s base=%s status=%s -> will try next", method, path, base, resp.Status)
			lastErr = fmt.Errorf("%s returned server error %s", base, resp.Status)
			continue
		}

		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				return err
			}
		}
		log.Printf("[client] request %s %s base=%s status=%s dur=%s", method, path, base, resp.Status, time.Since(start))
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
	log.Printf("[peer] id=%s dir=%s bind=%s addr=%s servers=%v", c.peerID, c.SharedDir, addr, c.PeerAddr, c.Servers)
	return http.ListenAndServe(addr, mux)
}

func (c *Client) backgroundLoop(stop <-chan struct{}) {
	// Register immediately
	if err := c.registerNow(); err != nil {
		log.Printf("initial register error: %v", err)
	}

	// Then, start the heartbeat loop.
	// The heartbeat interval should be less than the server's peerTTL.
	// A good value is around 2/3 of the TTL.
	// Keep heartbeat comfortably below server TTL (45s)
	heartbeatTicker := time.NewTicker(20 * time.Second)
	defer heartbeatTicker.Stop()

	for {
		select {
		case <-heartbeatTicker.C:
			if err := c.heartbeatNow(); err != nil {
				log.Printf("heartbeat error: %v", err)
				// If heartbeat fails, try to re-register
				if err := c.registerNow(); err != nil {
					log.Printf("re-register after heartbeat failure error: %v", err)
				}
			}
		case <-stop:
			return
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
		log.Printf("[client] register error: %v", err)
		return err
	}
	log.Printf("[client] register ok tasks=%d", len(resp.Tasks))
	c.processTasks(resp.Tasks)
	return nil
}

func (c *Client) heartbeatNow() error {
	req := shared.HeartbeatRequest{PeerID: c.peerIDFromAddr()}
	var resp shared.HeartbeatResponse
	if err := c.doRequest("POST", "/heartbeat", req, &resp); err != nil {
		log.Printf("[client] heartbeat error: %v", err)
		return err
	}
	if !resp.OK {
		log.Printf("[client] heartbeat not OK -> re-register")
		return c.registerNow()
	}
	log.Printf("[client] heartbeat ok tasks=%d", len(resp.Tasks))
	c.processTasks(resp.Tasks)
	return nil
}

// In client/client.go

func (c *Client) processTasks(tasks []shared.ReplicationTask) {
	for _, t := range tasks {
		// NEW: Run in background so we don't block the heartbeat loop
		go func(task shared.ReplicationTask) {
			log.Printf("[client] replication: pulling file=%s from=%s", task.File.Name, task.SourcePeer.Addr)
			if err := c.pullFile(task.SourcePeer.Addr, task.File.Name); err != nil {
				log.Printf("[client] replication failed file=%s err=%v", task.File.Name, err)
				return
			}
			// Announce
			if err := c.doRequest("POST", "/announce", shared.AnnounceRequest{
				Peer: shared.Peer{ID: c.peerIDFromAddr(), Addr: c.PeerAddr},
				File: task.File,
			}, nil); err != nil {
				log.Printf("[client] replication announce failed file=%s err=%v", task.File.Name, err)
				return
			}
			log.Printf("[client] replication: announced file=%s", task.File.Name)
		}(t)
	}
}

// --- NEW: Update File Workflow (Lease -> Edit -> Announce) ---

func (c *Client) UpdateFile(filename string) error {
	// 1. Acquire Lease
	log.Printf("[client] update: requesting lease for file=%s", filename)
	leaseReq := shared.LeaseRequest{PeerID: c.peerIDFromAddr(), FileName: filename}
	var leaseResp shared.LeaseResponse
	if err := c.doRequest("POST", "/lease", leaseReq, &leaseResp); err != nil {
		return err
	}
	if !leaseResp.Granted {
		return fmt.Errorf("lease denied: %s", leaseResp.Message)
	}
	log.Printf("[client] update: lease acquired exp=%s", leaseResp.Expiration.Format(time.RFC3339))

	// 2. Simulate "Edit" by just reading current stats and incrementing version
	path := filepath.Join(c.SharedDir, filename)
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("file not found locally: %v", err)
	}

	// In a real app, you'd fetch the current version from server first.
	// Here we just use a timestamp based version for monotonicity
	newVersion := time.Now().UTC().UnixNano()

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
	log.Printf("[client] update: announced file=%s version=%d", filename, newVersion)
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
	http.ServeContent(w, r, name, time.Now().UTC(), f)
}

func (c *Client) Search(query string) error {
	path := "/search?q=" + url.QueryEscape(query)
	var sr shared.SearchResponse
	if err := c.doRequest("GET", path, nil, &sr); err != nil {
		return err
	}
	if len(sr.Matches) == 0 {
		log.Printf("[client] search: no matches for %q", query)
		return nil
	}
	for _, m := range sr.Matches {
		var hosts []string
		for _, p := range m.Peers {
			hosts = append(hosts, p.Addr)
		}
		log.Printf("[client] search: %s (v%d, %d bytes) => %s", m.File.Name, m.File.Version, m.File.Size, strings.Join(hosts, ", "))
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
		log.Printf("[client] get: no peers for file=%s", name)
		return errors.New("no peers hosting that file")
	}
	// Pick first peer
	src := match.Peers[0]
	if err := c.pullFile(src.Addr, name); err != nil {
		return err
	}
	// Announce ownership
	err := c.doRequest("POST", "/announce", shared.AnnounceRequest{Peer: shared.Peer{ID: c.peerIDFromAddr(), Addr: c.PeerAddr}, File: match.File}, nil)
	if err != nil {
		log.Printf("[client] get: announce failed file=%s err=%v", name, err)
		return err
	}
	log.Printf("[client] get: announced file=%s", name)
	return nil
}

func (c *Client) pullFile(peerBase, name string) error {
	u := strings.TrimRight(peerBase, "/") + "/files/" + url.PathEscape(name)

	// NEW: Use a client with timeout
	client := http.Client{Timeout: 10 * time.Second}
	log.Printf("[client] download: GET %s", u)
	resp, err := client.Get(u)

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
	if err == nil {
		log.Printf("[client] download: saved file=%s to=%s", name, path)
	}
	return err
}

func (c *Client) ListLocal() error {
	files, err := c.scanFiles()
	if err != nil {
		return err
	}
	for _, f := range files {
		log.Printf("[client] local: %s (%d bytes)", f.Name, f.Size)
	}
	return nil
}
