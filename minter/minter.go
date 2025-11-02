// Binary minter provides a Kubernetes service account token minting service.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"cloud.google.com/go/storage"
)

const (
	tokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
)

func gcsDownload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	b := q.Get("bucket")
	o := q.Get("object")

	if os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") == "" {
		http.Error(w, "unable to serve GCS download because server is not configured with the required credentials", http.StatusInternalServerError)
		return
	}

	if b == "" {
		http.Error(w, "required URL query paramter 'bucket' was not provided", http.StatusBadRequest)
		return
	}
	if o == "" {
		http.Error(w, "required URL query paramter 'bucket' was not provided", http.StatusBadRequest)
		return
	}

	ctx := context.Background()
	client, err := storage.NewClient(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf("unable to initialize GCS client: %v", err), http.StatusInternalServerError)
		return
	}

	or, err := client.Bucket(b).Object(o).NewReader(ctx)
	if err != nil {
		log.Printf("GCS Download error: Bucket %q, object %q: %v", b, o, err)
		if errors.Is(err, storage.ErrObjectNotExist) {
			http.Error(w, fmt.Sprintf("bucket %q, object %q does not exist", b, o), http.StatusNotFound)
		}

		http.Error(w, fmt.Sprintf("unable to initialize reading bucket %q, object %q", b, o), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	if _, err := io.Copy(w, or); err != nil {
		log.Printf("GCS error streaming to client: Bucket %q, object %q: %v", b, o, err)
		http.Error(w, fmt.Sprintf("unable to stream contents of bucket %q, object %o", b, o), http.StatusInternalServerError)
	}
}

func getToken(w http.ResponseWriter, r *http.Request) {
	log.Printf("Token request: %v %v", r.Method, r.URL.Path)
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write(token)
}

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		rt := q.Get("type")
		switch rt {
		case "":
			getToken(w, r)
		case "token":
			getToken(w, r)
		case "gcs":
			gcsDownload(w, r)
		default:
			http.Error(w, fmt.Sprintf("unknown request type %q", rt), http.StatusBadRequest)
		}
	})
	addr := ":8080"
	log.Println("Starting minting server at", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
