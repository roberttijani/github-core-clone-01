package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var gitRouteRegex = regexp.MustCompile(`^/([^/]+)/([^/]+)\.git/(info/refs|git-upload-pack|git-receive-pack)$`)

func main() {
	portFlag := flag.Int("port", 0, "port to listen on")
	flag.Parse()

	port := *portFlag
	if port == 0 {
		if envPort := os.Getenv("PORT"); envPort != "" {
			if p, err := strconv.Atoi(envPort); err == nil {
				port = p
			}
		}
	}

	login := os.Getenv("MAKEFUN_FORGE_LOGIN")
	if login == "" {
		login = "adaiversen"
	}
	store := NewForgeStore(login)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		method := r.Method

		// 1. Health Probe
		if path == "/healthz" && method == http.MethodGet {
			sendJSON(w, http.StatusOK, map[string]bool{"ok": true}, nil)
			return
		}

		// 2. /done endpoint
		if path == "/done" && method == http.MethodPost {
			sendJSON(w, http.StatusOK, map[string]bool{"ok": true}, nil)
			go func() {
				time.Sleep(50 * time.Millisecond)
				os.Exit(0)
			}()
			return
		}

		authHeader := r.Header.Get("Authorization")
		hasToken := strings.HasPrefix(authHeader, "Bearer ")

		// 3. Git Smart-HTTP Surface
		if strings.Contains(path, ".git/") {
			matches := gitRouteRegex.FindStringSubmatch(path)
			if len(matches) == 4 {
				owner := matches[1]
				repoName := matches[2]
				gitAction := matches[3]
				handleGitHTTP(w, r, store, owner, repoName, gitAction)
				return
			}
		}

		// 4. MCP Surface (/mcp)
		if path == "/mcp" && method == http.MethodPost {
			handleMCP(w, r, store, hasToken)
			return
		}

		// 5. GraphQL Surface (/api/graphql)
		if path == "/api/graphql" && method == http.MethodPost {
			handleGraphQL(w, r, store, hasToken)
			return
		}

		// 6. REST v3 API Surface (/api/v3...)
		if strings.HasPrefix(path, "/api/v3") {
			restPath := strings.TrimPrefix(path, "/api/v3")
			if restPath == "" {
				restPath = "/"
			}
			restPath = strings.TrimRight(restPath, "/")
			if restPath == "" {
				restPath = "/"
			}
			handleREST(w, r, store, restPath, hasToken)
			return
		}

		// Default 404
		sendJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"}, nil)
	})

	addr := fmt.Sprintf("0.0.0.0:%d", port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to listen on %s: %v\n", addr, err)
		os.Exit(1)
	}

	actualPort := listener.Addr().(*net.TCPAddr).Port
	fmt.Printf("READY %d\n", actualPort)
	os.Stdout.Sync()

	server := &http.Server{
		Handler: handler,
	}
	server.SetKeepAlivesEnabled(true)
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
	}
}
