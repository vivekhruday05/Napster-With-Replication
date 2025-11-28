package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/url"
	"strings"
)

func main() {
	cmd := flag.String("cmd", "serve", "command: serve | search | get | list | update")
	// UPDATED: Clarify that this can be a list
	server := flag.String("server", "http://localhost:8080", "comma-separated server URLs (e.g. http://localhost:8080,http://localhost:8081)")
	sharedDir := flag.String("dir", "./shared", "directory to share/store files")
	peerAddr := flag.String("addr", "", "this peer's public HTTP base URL; default auto-detect")
	bindAddr := flag.String("bind", "", "address to bind the peer file server (host:port); default auto-detect")
	query := flag.String("q", "", "query for search")
	filename := flag.String("file", "", "file name for get or update")
	flag.Parse()

	// NEW: Parse comma-separated servers for failover
	serverList := strings.Split(*server, ",")
	for i := range serverList {
		serverList[i] = strings.TrimSpace(serverList[i])
	}

	// Auto-select IP and port if not provided
	resolvedPeerAddr := strings.TrimSpace(*peerAddr)
	resolvedBindAddr := strings.TrimSpace(*bindAddr)

	if resolvedPeerAddr == "" || resolvedBindAddr == "" {
		ip := localIP()
		port := 0
		if resolvedBindAddr == "" {
			l, err := net.Listen("tcp", ":0")
			if err == nil {
				if ta, ok := l.Addr().(*net.TCPAddr); ok {
					port = ta.Port
				}
				_ = l.Close()
			}
			if port == 0 {
				port = 9000
			}
			resolvedBindAddr = fmt.Sprintf(":%d", port)
		} else {
			if strings.HasPrefix(resolvedBindAddr, ":") {
				p := strings.TrimPrefix(resolvedBindAddr, ":")
				if p != "" {
					if v, err := net.LookupPort("tcp", p); err == nil {
						port = v
					}
				}
			} else {
				if h, p, err := net.SplitHostPort(resolvedBindAddr); err == nil {
					_ = h
					if v, err := net.LookupPort("tcp", p); err == nil {
						port = v
					}
				}
			}
		}

		if resolvedPeerAddr == "" {
			if port == 0 {
				port = 9000
			}
			resolvedPeerAddr = fmt.Sprintf("http://%s:%d", ip, port)
		} else {
			if u, err := url.Parse(resolvedPeerAddr); err == nil && u.Host != "" && !strings.Contains(u.Host, ":") {
				if port == 0 {
					port = 9000
				}
				u.Host = fmt.Sprintf("%s:%d", u.Host, port)
				resolvedPeerAddr = u.String()
			}
		}

		log.Printf("Using peer public address %s and bind address %s", resolvedPeerAddr, resolvedBindAddr)
	}

	// UPDATED: Initialize with Servers slice instead of single ServerBase
	c := &Client{
		Servers:   serverList,
		SharedDir: *sharedDir,
		PeerAddr:  resolvedPeerAddr,
		BindAddr:  resolvedBindAddr,
	}
	if err := c.EnsureDir(); err != nil {
		log.Fatal(err)
	}

	switch *cmd {
	case "serve":
		log.Fatal(c.Serve())
	case "search":
		if *query == "" {
			log.Fatal("-q required for search")
		}
		if err := c.Search(*query); err != nil {
			log.Fatal(err)
		}
	case "get":
		if *filename == "" {
			log.Fatal("-file required for get")
		}
		if err := c.Get(*filename); err != nil {
			log.Fatal(err)
		}
	case "list":
		if err := c.ListLocal(); err != nil {
			log.Fatal(err)
		}
	case "update": // NEW CASE
		if *filename == "" {
			log.Fatal("-file required for update")
		}
		if err := c.UpdateFile(*filename); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatalf("unknown cmd: %s", *cmd)
	}
}

func localIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
	}
	for _, iface := range ifaces {
		if (iface.Flags&net.FlagUp) == 0 || (iface.Flags&net.FlagLoopback) != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip = ip.To4()
			if ip == nil {
				continue
			}
			return ip.String()
		}
	}
	return "127.0.0.1"
}