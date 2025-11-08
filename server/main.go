package main

import (
	"log"
	"net"
	"net/http"
	"os"
	"strings"
)

func main() {
	addr := os.Getenv("NAPSTER_SERVER_ADDR")

	srv := NewServer()
	mux := http.NewServeMux()
	registerRoutes(mux, srv)

	if strings.TrimSpace(addr) == "" {
		// Auto-detect: bind to a free port on all interfaces and print a reachable URL
		l, err := net.Listen("tcp", ":0")
		if err != nil {
			log.Fatal(err)
		}
		var port int
		if ta, ok := l.Addr().(*net.TCPAddr); ok {
			port = ta.Port
		}
		ip := localIP()
		log.Printf("Napster server listening on %s (public URL: http://%s:%d)", l.Addr().String(), ip, port)
		if err := http.Serve(l, mux); err != nil {
			log.Fatal(err)
		}
		return
	}

	// Use provided address
	log.Printf("Napster server listening on %s", addr)
	if host, port, err := net.SplitHostPort(addr); err == nil {
		if host == "" || host == "0.0.0.0" || host == "::" {
			ip := localIP()
			log.Printf("Napster server public URL: http://%s:%s", ip, port)
		}
	}
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

// localIP mirrors the client's helper to pick a non-loopback IPv4.
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
			if v4 := ip.To4(); v4 != nil {
				return v4.String()
			}
		}
	}
	return "127.0.0.1"
}
