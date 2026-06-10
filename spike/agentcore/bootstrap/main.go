// Command bootstrap is the AgentCore Runtime container entrypoint for the
// naozhi cloud-sandbox Phase 0 spike (docs/rfc/agentcore-cloud-sandbox.md §4.1).
//
// It exposes the two endpoints the AgentCore Runtime contract requires:
//
//	GET  /ping        → {"status":"Healthy"}
//	POST /invocations → materialize payload → spawn claude CLI → SSE stream back
//
// Spike-only code: NOT part of the naozhi binary, never imported by internal/.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080" // AgentCore Runtime contract port
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "Healthy"})
	})
	mux.HandleFunc("POST /invocations", handleInvocation)

	log.Printf("bootstrap: listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("bootstrap: server failed: %v", err)
	}
}
