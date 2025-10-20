// Binary minter provides a Kubernetes service account token minting service.
package main

import (
	"log"
	"net/http"
	"os"
)

const (
	tokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Token request: %v %v", r.Method, r.URL.Path)
		token, err := os.ReadFile(tokenPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write(token)
	})
	addr := ":8080"
	log.Println("Starting minting server at", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
