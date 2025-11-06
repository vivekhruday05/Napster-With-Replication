package main

import (
	"log"
	"net/http"
	"os"
)

func main() {
	addr := ":8080"
	if v := os.Getenv("NAPSTER_SERVER_ADDR"); v != "" {
		addr = v
	}

	srv := NewServer()
	mux := http.NewServeMux()
	registerRoutes(mux, srv)

	log.Printf("Napster server listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
