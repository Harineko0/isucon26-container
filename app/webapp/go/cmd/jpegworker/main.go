package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"example.com/isureal-company/isureal/internal/jpegworker"
)

func main() {
	addr := os.Getenv("ISUREAL_JPEG_LISTEN")
	credential := jpegworker.Credential(os.Getenv("ISUREAL_DB_PASSWORD"))
	if addr == "" || credential == "" {
		log.Fatal("missing JPEG worker listen address or credential")
	}
	server := http.Server{
		Addr: addr, Handler: jpegworker.Handler(credential),
		ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second,
		WriteTimeout: 3 * time.Second, IdleTimeout: time.Minute,
	}
	log.Fatal(server.ListenAndServe())
}
