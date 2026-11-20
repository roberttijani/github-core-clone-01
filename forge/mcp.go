package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func handleMCP(w http.ResponseWriter, r *http.Request, store *ForgeStore, hasToken bool) {
	if !hasToken {
		sendJSON(w, http.StatusUnauthorized, map[string]string{"message": "Requires authentication"}, nil)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]any{
			"jsonrpc": "2.0",
			"id":      nil,
			"error":   map[string]any{"code": -32700, "message": "Parse error"},
		}, nil)
		return
	}

	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]any{
			"jsonrpc": "2.0",
			"id":      nil,
			"error":   map[string]any{"code": -32700, "message": "Parse error"},
		}, nil)
		return
	}

	id := msg["id"]
	method, _ := msg["method"].(string)

	if method == "server/discover" {
		sendJSON(w, http.StatusOK, map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"result": map[string]any{
				"supportedVersions": []string{"2026-07-28", "2025-11-25"},
				"serverInfo":        map[string]string{"name": "makefun-forge", "version": "1.0.0"},
				"capabilities":      map[string]any{"tools": map[string]any{}},
			},
		}, nil)
		return
	}

	if method == "initialize" {
		sendJSON(w, http.StatusOK, map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"result": map[string]any{
				"protocolVersion": "2025-11-25",
				"serverInfo":      map[string]string{"name": "makefun-forge", "version": "1.0.0"},
				"capabilities":    map[string]any{"tools": map[string]any{}},
			},
		}, nil)
		return
	}

	if method == "notifications/initialized" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if method == "tools/list" {
		tools := []map[string]any{
			{
				"name":        "create_repo",
				"description": "Create a new repository",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":        map[string]string{"type": "string"},
						"description": map[string]string{"type": "string"},
						"private":     map[string]string{"type": "boolean"},
					},
					"required": []string{"name"},
				},
			},
			{
				"name":        "get_repo",
				"description": "Get repository details",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"owner": map[string]string{"type": "string"},
						"repo":  map[string]string{"type": "string"},
					},
					"required": []string{"owner", "repo"},
				},
			},
			{
				"name":        "create_issue",
				"description": "Create an issue in a repository",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"owner": map[string]string{"type": "string"},
						"repo":  map[string]string{"type": "string"},
						"title": map[string]string{"type": "string"},
						"body":  map[string]string{"type": "string"},
					},
					"required": []string{"owner", "repo", "title"},
				},
			},
			{
				"name":        "list_issues",
				"description": "List issues in a repository",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"owner": map[string]string{"type": "string"},
						"repo":  map[string]string{"type": "string"},
						"state": map[string]string{"type": "string"},
					},
					"required": []string{"owner", "repo"},
				},
			},
			{
				"name":        "create_pull_request",
				"description": "Create a pull request",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"owner": map[string]string{"type": "string"},
						"repo":  map[string]string{"type": "string"},
						"title": map[string]string{"type": "string"},
						"head":  map[string]string{"type": "string"},
						"base":  map[string]string{"type": "string"},
					},
					"required": []string{"owner", "repo", "title", "head", "base"},
				},
			},
			{
				"name":        "merge_pull_request",
				"description": "Merge a pull request",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"owner":       map[string]string{"type": "string"},
						"repo":        map[string]string{"type": "string"},
						"pull_number": map[string]string{"type": "integer"},
					},
					"required": []string{"owner", "repo", "pull_number"},
				},
			},
		}
		sendJSON(w, http.StatusOK, map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"result":  map[string]any{"tools": tools},
		}, nil)
		return
	}

	if method == "tools/call" {
		params, _ := msg["params"].(map[string]any)
		toolName, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = make(map[string]any)
		}

		extractRepo := func() (*Repo, string, string) {
			owner, _ := args["owner"].(string)
			repoName, _ := args["repo"].(string)
			if fn, ok := args["full_name"].(string); ok && fn != "" {
				sp := strings.Split(fn, "/")
				if len(sp) == 2 {
					owner = sp[0]
					repoName = sp[1]
				}
			}
			if owner == "" {
				owner = store.Viewer.Login
			}
			return store.GetRepo(owner, repoName), owner, repoName
		}

		if toolName == "create_repo" {
			name, _ := args["name"].(string)
			if name == "" {
				sendJSON(w, http.StatusOK, map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result":  map[string]any{"isError": true, "content": []map[string]string{{"type": "text", "text": "Missing name"}}},
				}, nil)
				return
			}
			isPrivate := true
			if p, ok := args["private"].(bool); ok {
				isPrivate = p
			}
			var desc *string
			if d, ok := args["description"].(string); ok {
				desc = &d
			}
			repo := store.CreateRepo(name, isPrivate, desc)
			data := map[string]any{"id": repo.ID, "name": repo.Name, "private": repo.Private, "owner": repo.Owner}
			strData, _ := json.Marshal(data)
			sendJSON(w, http.StatusOK, map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"content":           []map[string]string{{"type": "text", "text": string(strData)}},
					"structuredContent": data,
				},
			}, nil)
			return
		}

		if toolName == "get_repo" {
			repo, _, _ := extractRepo()
			if repo == nil {
				sendJSON(w, http.StatusOK, map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result":  map[string]any{"isError": true, "content": []map[string]string{{"type": "text", "text": "Not found"}}},
				}, nil)
				return
			}
			data := map[string]any{"id": repo.ID, "name": repo.Name, "default_branch": repo.DefaultBranch, "private": repo.Private, "owner": repo.Owner}
			strData, _ := json.Marshal(data)
			sendJSON(w, http.StatusOK, map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"content":           []map[string]string{{"type": "text", "text": string(strData)}},
					"structuredContent": data,
				},
			}, nil)
			return
		}

		if toolName == "create_issue" {
			title, _ := args["title"].(string)
			repo, _, _ := extractRepo()
			if title == "" || repo == nil {
				sendJSON(w, http.StatusOK, map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result":  map[string]any{"isError": true, "content": []map[string]string{{"type": "text", "text": "Repo not found or missing title"}}},
				}, nil)
				return
			}
			repo.Mu.Lock()
			repo.IssueSeq++
			num := repo.IssueSeq
			body, _ := args["body"].(string)
			now := time.Now().UTC().Format(time.RFC3339)
			iss := &Issue{
				ID:        num,
				Number:    num,
				Title:     title,
				Body:      body,
				State:     "open",
				Labels:    []Label{},
				CreatedAt: now,
				UpdatedAt: now,
			}
			iss.CachedJSON = serializeIssue(iss)
			repo.Issues[num] = iss
			repo.Mu.Unlock()

			data := map[string]any{"id": iss.ID, "number": iss.Number, "title": iss.Title, "body": iss.Body, "state": iss.State}
			strData, _ := json.Marshal(data)
			sendJSON(w, http.StatusOK, map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"content":           []map[string]string{{"type": "text", "text": string(strData)}},
					"structuredContent": data,
				},
			}, nil)
			return
		}

		if toolName == "list_issues" {
			repo, _, _ := extractRepo()
			if repo == nil {
				sendJSON(w, http.StatusOK, map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result":  map[string]any{"isError": true, "content": []map[string]string{{"type": "text", "text": "Repo not found"}}},
				}, nil)
				return
			}
			state, _ := args["state"].(string)
			repo.Mu.RLock()
			var issues []*Issue
			for _, iss := range repo.Issues {
				if state == "" || state == "all" || iss.State == state {
					issues = append(issues, iss)
				}
			}
			repo.Mu.RUnlock()

			strData, _ := json.Marshal(issues)
			sendJSON(w, http.StatusOK, map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"content":           []map[string]string{{"type": "text", "text": string(strData)}},
					"structuredContent": issues,
				},
			}, nil)
			return
		}

		if toolName == "create_pull_request" {
			title, _ := args["title"].(string)
			head, _ := args["head"].(string)
			base, _ := args["base"].(string)
			repo, _, _ := extractRepo()
			if title == "" || head == "" || base == "" || repo == nil {
				sendJSON(w, http.StatusOK, map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result":  map[string]any{"isError": true, "content": []map[string]string{{"type": "text", "text": "Missing argument or repo not found"}}},
				}, nil)
				return
			}
			repo.Mu.Lock()
			repo.PullSeq++
			num := repo.PullSeq
			headSha := repo.Refs["refs/heads/"+head]
			if headSha == "" {
				headSha = ZeroOID
			}
			baseSha := repo.Refs["refs/heads/"+base]
			if baseSha == "" {
				baseSha = ZeroOID
			}
			pull := &PullRequest{
				ID:            num,
				Number:        num,
				Title:         title,
				State:         "open",
				Merged:        false,
				Head:          HeadBase{Ref: head, SHA: headSha},
				Base:          HeadBase{Ref: base, SHA: baseSha},
				HeadCommitSHA: headSha,
				BaseCommitSHA: baseSha,
			}
			repo.Pulls[num] = pull
			repo.Mu.Unlock()

			data := map[string]any{"id": pull.ID, "number": pull.Number, "title": pull.Title, "state": pull.State, "head": pull.Head, "base": pull.Base}
			strData, _ := json.Marshal(data)
			sendJSON(w, http.StatusOK, map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"content":           []map[string]string{{"type": "text", "text": string(strData)}},
					"structuredContent": data,
				},
			}, nil)
			return
		}

		if toolName == "merge_pull_request" {
			repo, _, _ := extractRepo()
			if repo == nil {
				sendJSON(w, http.StatusOK, map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result":  map[string]any{"isError": true, "content": []map[string]string{{"type": "text", "text": "Repo not found"}}},
				}, nil)
				return
			}
			pullNum := 0
			if pn, ok := args["pull_number"].(float64); ok {
				pullNum = int(pn)
			} else if n, ok := args["number"].(float64); ok {
				pullNum = int(n)
			}
			if pullNum == 0 {
				sendJSON(w, http.StatusOK, map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result":  map[string]any{"isError": true, "content": []map[string]string{{"type": "text", "text": "Missing pull_number"}}},
				}, nil)
				return
			}

			repo.Mu.Lock()
			pull, ok := repo.Pulls[pullNum]
			if !ok {
				repo.Mu.Unlock()
				sendJSON(w, http.StatusOK, map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result":  map[string]any{"isError": true, "content": []map[string]string{{"type": "text", "text": "Pull not found"}}},
				}, nil)
				return
			}
			var treeOid string
			if hObj, ok := repo.Objects[pull.Head.SHA]; ok && hObj.Type == TypeCommit {
				cv, _ := ParseCommit(hObj.Data)
				treeOid = cv.Tree
			}
			baseBranchRef := "refs/heads/" + pull.Base.Ref
			baseSha := repo.Refs[baseBranchRef]
			if baseSha == "" {
				baseSha = pull.Base.SHA
			}
			squashSha := repo.CreateCommit(treeOid, []string{baseSha}, fmt.Sprintf("Merge pull request #%d from %s", pullNum, pull.Head.Ref))
			repo.Refs[baseBranchRef] = squashSha
			pull.State = "closed"
			pull.Merged = true
			now := time.Now().UTC().Format(time.RFC3339)
			pull.MergedAt = &now
			pull.MergeCommitSHA = &squashSha
			repo.Mu.Unlock()

			data := map[string]any{"sha": squashSha, "merged": true, "message": "Pull Request successfully merged"}
			strData, _ := json.Marshal(data)
			sendJSON(w, http.StatusOK, map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"content":           []map[string]string{{"type": "text", "text": string(strData)}},
					"structuredContent": data,
				},
			}, nil)
			return
		}

		sendJSON(w, http.StatusOK, map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"error":   map[string]any{"code": -32601, "message": "Method " + toolName + " not found"},
		}, nil)
		return
	}

	sendJSON(w, http.StatusOK, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": -32601, "message": "Method not found"},
	}, nil)
}
