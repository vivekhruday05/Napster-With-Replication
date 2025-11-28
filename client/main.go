package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"strings"

	shared "github.com/vivekhruday05/Napster-With-Replication/pkg/shared"
)

func main() {
	servers := flag.String("server", "http://localhost:8080", "comma-separated list of server addresses")
	sharedDir := flag.String("dir", "shared", "folder to share files from")
	bindAddr := flag.String("bind", ":9000", "address to bind the peer server to")
	peerAddr := flag.String("addr", "", "public address of this peer (e.g. http://1.2.3.4:9000)")
	cmd := flag.String("cmd", "serve", "serve, search, get, update, or list")
	flag.Parse()

	if *peerAddr == "" {
		ip, err := shared.GetLocalIP()
		if err != nil {
			log.Fatalf("Could not determine local IP: %v", err)
		}
		_, port, _ := net.SplitHostPort(*bindAddr)
		if port == "" {
			port = "9000" // Default if not specified in bind
		}
		*peerAddr = fmt.Sprintf("http://%s:%s", ip, port)
	}

	c := &Client{
		Servers:   strings.Split(*servers, ","),
		SharedDir: *sharedDir,
		BindAddr:  *bindAddr,
		PeerAddr:  *peerAddr,
	}
	if err := c.EnsureDir(); err != nil {
		log.Fatal(err)
	}

	switch *cmd {
	case "serve":
		log.Fatal(c.Serve())
	case "search":
		query := strings.Join(flag.Args(), " ")
		if query == "" {
			log.Fatal("search query required")
		}
		if err := c.Search(query); err != nil {
			log.Fatal(err)
		}
	case "get":
		name := flag.Arg(0)
		if name == "" {
			log.Fatal("get requires a filename")
		}
		if err := c.Get(name); err != nil {
			log.Fatal(err)
		}
	case "update":
		name := flag.Arg(0)
		if name == "" {
			log.Fatal("update requires a filename")
		}
		if err := c.UpdateFile(name); err != nil {
			log.Fatal(err)
		}
	case "list":
		if err := c.ListLocal(); err != nil {
			log.Fatalf("list error: %v", err)
		}
	default:
		log.Fatalf("unknown command %q", *cmd)
	}
}
