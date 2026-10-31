package main

import (
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"

	"example.com/isureal-company/isureal"
)

// 2026-09-23

func main() {
	if os.Getenv("ISUREAL_PORT") == "" {
		go http.ListenAndServe("127.0.0.1:6060", nil)
	}
	config := isureal.ConfigFromEnv()
	server := isureal.NewServer(config)
	port := os.Getenv("ISUREAL_PORT")
	if port == "" {
		port = "8080"
	}
	addr := net.JoinHostPort(os.Getenv("ISUREAL_LISTEN_HOST"), port)
	if err := server.Start(addr); err != nil {
		panic("failed to start server: " + err.Error())
	}
}
