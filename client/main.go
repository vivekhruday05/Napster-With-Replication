package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"

	shared "github.com/vivekhruday05/Napster-With-Replication/pkg/shared"
)

func main() {
	servers := flag.String("server", "http://localhost:8080", "comma-separated list of server addresses")
	sharedDir := flag.String("dir", "shared", "folder to share files from")
	bindAddr := flag.String("bind", ":9000", "address to bind the peer server to")
	peerAddr := flag.String("addr", "", "public address of this peer (e.g. http://1.2.3.4:9000)")
	logDir := flag.String("logdir", "logs", "directory to write peer log file")
	cmd := flag.String("cmd", "serve", "serve, search, get, update, delete, or list")
	flag.Parse()

	// User mistake recovery: if running 'serve' and forgot -server but supplied addresses as positional arg.
	if *cmd == "serve" && *servers == "http://localhost:8080" { // still default
		args := flag.Args()
		if len(args) > 0 {
			candidate := args[0]
			// Heuristic: looks like a URL list if contains 'http://' and maybe comma
			if strings.Contains(candidate, "http://") || strings.Contains(candidate, "https://") {
				log.Printf("[warning] treating positional argument %q as -server value; please use -server explicitly", candidate)
				*servers = candidate
				// Remove it from args so subsequent logic (search/get/update) isn't confused in other commands
				if len(args) > 1 {
					// Reconstruct remaining args in flag package isn't possible; we just rely on flag.Args() usage below.
				}
			}
		}
	}

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

	// Setup logging to file + stdout
	if err := os.MkdirAll(*logDir, 0o755); err != nil {
		log.Fatalf("failed creating log dir: %v", err)
	}
	peerID := c.peerIDFromAddr()
	logPath := fmt.Sprintf("%s/client-%s.log", *logDir, peerID)
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatalf("failed opening log file: %v", err)
	}
	// MultiWriter to keep CLI output while persisting logs
	log.SetOutput(io.MultiWriter(os.Stdout, f))
	log.Printf("[init] logging to %s", logPath)
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
	case "delete":
		name := flag.Arg(0)
		if name == "" {
			log.Fatal("delete requires a filename")
		}
		if err := c.DeleteFile(name); err != nil {
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
