package main

import (
	"log"
	"net/http"
	"os"

	"github.com/kirimemail/load-testing/internal/generator"
)

func main() {
	id := getenv("GENERATOR_ID", "gen-1")
	addr := getenv("MANAGEMENT_ADDR", ":8080")
	srv := generator.NewServer(id)
	log.Printf("generator %s listening on %s", id, addr)
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
