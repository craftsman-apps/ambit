//go:build ignore

// gitserve serves the git repositories under a directory over HTTP (git's smart protocol, read only), for the
// GitOps test of ambit init (test/gitops/init-argocd.sh). Argo CD on kind reads from it.
//
//	go run test/gitops/gitserve.go <directory> <address>
package main

import (
	"log"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
)

func main() {
	if len(os.Args) != 3 {
		log.Fatal("usage: go run test/gitops/gitserve.go <directory> <address>")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		log.Fatal(err)
	}
	h := &cgi.Handler{Path: git, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + os.Args[1], "GIT_HTTP_EXPORT_ALL=1"}}
	log.Fatal(http.ListenAndServe(os.Args[2], h))
}
