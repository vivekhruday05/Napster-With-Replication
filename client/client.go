package main

import (
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
	ServerBase string
	SharedDir  string
	PeerAddr   string // public URL like http://host:port
	BindAddr   string // local listen address host:port, e.g. :9000 or 0.0.0.0:9000
	peerID     string
}

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

func (c *Client) Serve() error {
	if c.peerID == "" {
		c.peerID = c.peerIDFromAddr()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/files", c.handleListFiles)
	mux.HandleFunc("/files/", c.handleGetFile)

	// kick off background register/heartbeat loop
	stop := make(chan struct{})
	go c.backgroundLoop(stop)
	defer close(stop)

	// determine listen address from BindAddr (separate from public PeerAddr)
	addr := c.BindAddr
	if strings.TrimSpace(addr) == "" {
		addr = ":9000"
	}
	log.Printf("Peer %s serving files from %s on %s", c.peerID, c.SharedDir, addr)
	return http.ListenAndServe(addr, mux)
}

func (c *Client) backgroundLoop(stop <-chan struct{}) {
	// initial register and then heartbeat
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
	if err := c.postJSON("/register", req, &resp); err != nil {
		return err
	}
	c.processTasks(resp.Tasks)
	return nil
}

func (c *Client) heartbeatNow() error {
	req := shared.HeartbeatRequest{PeerID: c.peerIDFromAddr()}
	var resp shared.HeartbeatResponse
	if err := c.postJSON("/heartbeat", req, &resp); err != nil {
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
		// announce to server
		_ = c.postJSON("/announce", shared.AnnounceRequest{Peer: shared.Peer{ID: c.peerIDFromAddr(), Addr: c.PeerAddr}, File: t.File}, nil)
	}
}

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
	u := c.ServerBase + "/search?q=" + url.QueryEscape(query)
	resp, err := http.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("search failed: %s", resp.Status)
	}
	var sr shared.SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
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
		log.Printf("%s (%d bytes) => %s", m.File.Name, m.File.Size, strings.Join(hosts, ", "))
	}
	return nil
}

func (c *Client) Get(name string) error {
	match, err := c.lookupPeers(name)
	if err != nil {
		return err
	}
	if len(match.Peers) == 0 {
		return errors.New("no peers hosting that file")
	}
	// choose the first
	src := match.Peers[0]
	if err := c.pullFile(src.Addr, name); err != nil {
		return err
	}
	// announce
	return c.postJSON("/announce", shared.AnnounceRequest{Peer: shared.Peer{ID: c.peerIDFromAddr(), Addr: c.PeerAddr}, File: match.File}, nil)
}

func (c *Client) lookupPeers(name string) (shared.SearchMatch, error) {
	u := c.ServerBase + "/peers?file=" + url.QueryEscape(name)
	resp, err := http.Get(u)
	if err != nil {
		return shared.SearchMatch{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return shared.SearchMatch{}, fmt.Errorf("lookup failed: %s", resp.Status)
	}
	var m shared.SearchMatch
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return shared.SearchMatch{}, err
	}
	return m, nil
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

func (c *Client) postJSON(path string, in any, out any) error {
	b, _ := json.Marshal(in)
	resp, err := http.Post(c.ServerBase+path, "application/json", strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s", "POST", path, resp.Status)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
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
