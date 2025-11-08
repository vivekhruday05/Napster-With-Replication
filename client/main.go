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
	cmd := flag.String("cmd", "serve", "command: serve | search | get | list")
	server := flag.String("server", "http://localhost:8080", "central server base URL")
	sharedDir := flag.String("dir", "./shared", "directory to share/store files")
	// If -addr or -bind are omitted, we'll auto-detect a reachable local IP and a free port
	peerAddr := flag.String("addr", "", "this peer's public HTTP base URL (what others will use to reach you); default auto-detect")
	bindAddr := flag.String("bind", "", "address to bind the peer file server (host:port); default auto-detect a free port")
	query := flag.String("q", "", "query for search")
	filename := flag.String("file", "", "file name for get")
	flag.Parse()

	// Auto-select IP and port if not provided
	resolvedPeerAddr := strings.TrimSpace(*peerAddr)
	resolvedBindAddr := strings.TrimSpace(*bindAddr)

	if resolvedPeerAddr == "" || resolvedBindAddr == "" {
		ip := localIP()
		port := 0
		if resolvedBindAddr == "" {
			// pick a free port by asking the OS, then close and reuse immediately
			l, err := net.Listen("tcp", ":0")
			if err == nil {
				if ta, ok := l.Addr().(*net.TCPAddr); ok {
					port = ta.Port
				}
				_ = l.Close()
			}
			if port == 0 {
				// fallback to a common high port if needed
				port = 9000
			}
			resolvedBindAddr = fmt.Sprintf(":%d", port)
		} else {
			// Try to parse a port from provided bindAddr
			if strings.HasPrefix(resolvedBindAddr, ":") {
				// format ":1234"
				p := strings.TrimPrefix(resolvedBindAddr, ":")
				if p != "" {
					// ignore parse error; just leave port=0 if not numeric
					if v, err := net.LookupPort("tcp", p); err == nil {
						port = v
					}
				}
			} else {
				// host:port
				if h, p, err := net.SplitHostPort(resolvedBindAddr); err == nil {
					_ = h
					if v, err := net.LookupPort("tcp", p); err == nil {
						port = v
					}
				}
			}
		}

		if resolvedPeerAddr == "" {
			// Default to reachable local IP with chosen/best-effort port
			if port == 0 {
				port = 9000
			}
			resolvedPeerAddr = fmt.Sprintf("http://%s:%d", ip, port)
		} else {
			// If user provided a URL but not port, add the detected/bind port
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

	c := &Client{ServerBase: *server, SharedDir: *sharedDir, PeerAddr: resolvedPeerAddr, BindAddr: resolvedBindAddr}
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
	default:
		log.Fatalf("unknown cmd: %s", *cmd)
	}
}

// localIP attempts to find a non-loopback IPv4 address for this machine.
// Preference order: first active, non-loopback interface with an IPv4 address.
// Falls back to 127.0.0.1 if none found.
func localIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
	}
	for _, iface := range ifaces {
		// Skip loopback or down
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
				continue // not IPv4
			}
			return ip.String()
		}
	}
	return "127.0.0.1"
}
