package main

import (
	"flag"
	"log"
)

func main() {
	cmd := flag.String("cmd", "serve", "command: serve | search | get | list")
	server := flag.String("server", "http://localhost:8080", "central server base URL")
	sharedDir := flag.String("dir", "./shared", "directory to share/store files")
	peerAddr := flag.String("addr", "http://localhost:9000", "this peer's public HTTP address")
	query := flag.String("q", "", "query for search")
	filename := flag.String("file", "", "file name for get")
	flag.Parse()

	c := &Client{ServerBase: *server, SharedDir: *sharedDir, PeerAddr: *peerAddr}
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
