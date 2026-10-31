package main

import (
	"net/http"
	"os"
	_ "net/http/pprof"

	"example.com/isureal-company/isureal"
)

// 2026-09-23

func main() {
	if os.Getenv("ISUREAL_PORT") == "" {
		go http.ListenAndServe("127.0.0.1:6060", nil)
	}
	config := isureal.ConfigFromEnv()
	server := isureal.NewServer(config)
	addr := ":8080"
	if port := os.Getenv("ISUREAL_PORT"); port != "" {
		addr = ":" + port
	}
	if err := server.Start(addr); err != nil {
		panic("failed to start server: " + err.Error())
	}
}
