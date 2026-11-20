package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

func sendJSON(w http.ResponseWriter, status int, body any, extraHeaders map[string]string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-GitHub-Request-Id", fmt.Sprintf("%x", time.Now().UnixNano()))
	for k, v := range extraHeaders {
		w.Header().Set(k, v)
	}
	data, _ := json.Marshal(body)
	data = append(data, '\n')
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	w.Write(data)
}

func readJSONBody(r *http.Request) (map[string]any, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return make(map[string]any), nil
	}
	var res map[string]any
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, err
	}
	return res, nil
}

func serializeIssue(i *Issue) []byte {
	labels := i.Labels
	if labels == nil {
		labels = []Label{}
	}
	data := map[string]any{
		"id":     i.ID,
		"number": i.Number,
		"title":  i.Title,
		"body":   i.Body,
		"state":  i.State,
		"labels": labels,
	}
	b, _ := json.Marshal(data)
	return append(b, '\n')
}

func handleREST(w http.ResponseWriter, r *http.Request, store *ForgeStore, restPath string, hasToken bool) {
	// Root /api/v3/
	if restPath == "/" {
		sendJSON(w, http.StatusOK, map[string]any{
			"current_user_url": "/api/v3/user",
			"rate_limit_url":   "/api/v3/rate_limit",
		}, map[string]string{"x-oauth-scopes": "repo, read:org"})
		return
	}

	// Meta /api/v3/meta
	if restPath == "/meta" {
		sendJSON(w, http.StatusOK, map[string]any{"installed_version": "1.0.0"}, nil)
		return
	}

	// User /api/v3/user
	if restPath == "/user" {
		if !hasToken {
			sendJSON(w, http.StatusUnauthorized, map[string]string{"message": "Requires authentication"}, nil)
			return
		}
		sendJSON(w, http.StatusOK, map[string]any{
			"login":   store.Viewer.Login,
			"id":      store.Viewer.ID,
			"node_id": "MDQ6VXNlcjEwMDA=",
			"name":    store.Viewer.Name,
			"email":   store.Viewer.Email,
		}, nil)
		return
	}

	// Check authentication for all remaining REST routes
	if !hasToken {
		sendJSON(w, http.StatusUnauthorized, map[string]string{"message": "Requires authentication"}, nil)
		return
	}

	// /api/v3/user/repos
	if restPath == "/user/repos" {
		if r.Method == http.MethodPost {
			parsed, err := readJSONBody(r)
			if err != nil {
				sendJSON(w, http.StatusBadRequest, map[string]string{"message": "Problems parsing JSON"}, nil)
				return
			}
			name, _ := parsed["name"].(string)
			if name == "" {
				sendJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Name is required"}, nil)
				return
			}
			if store.GetRepo(store.Viewer.Login, name) != nil {
				sendJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Repository already exists"}, nil)
				return
			}
			isPrivate := true
			if p, ok := parsed["private"].(bool); ok {
				isPrivate = p
			}
			var desc *string
			if d, ok := parsed["description"].(string); ok {
				desc = &d
			}
			repo := store.CreateRepo(name, isPrivate, desc)
			sendJSON(w, http.StatusCreated, map[string]any{
				"id":             repo.ID,
				"node_id":        repo.NodeID,
				"name":           repo.Name,
				"full_name":      repo.Owner.Login + "/" + repo.Name,
				"private":        repo.Private,
				"owner":          repo.Owner,
				"description":    repo.Description,
				"default_branch": repo.DefaultBranch,
			}, nil)
			return
		}

		if r.Method == http.MethodGet {
			perPage := 30
			if ppStr := r.URL.Query().Get("per_page"); ppStr != "" {
				if pp, err := strconv.Atoi(ppStr); err == nil && pp > 0 {
					perPage = pp
				}
			}
			page := 1
			if pStr := r.URL.Query().Get("page"); pStr != "" {
				if p, err := strconv.Atoi(pStr); err == nil && p > 0 {
					page = p
				}
			}

			store.Mu.RLock()
			var allRepos []map[string]any
			for _, repo := range store.Repos {
				allRepos = append(allRepos, map[string]any{
					"id":             repo.ID,
					"node_id":        repo.NodeID,
					"name":           repo.Name,
					"full_name":      repo.Owner.Login + "/" + repo.Name,
					"private":        repo.Private,
					"owner":          repo.Owner,
					"description":    repo.Description,
					"default_branch": repo.DefaultBranch,
				})
			}
			store.Mu.RUnlock()

			sort.Slice(allRepos, func(i, j int) bool {
				return allRepos[i]["id"].(int) < allRepos[j]["id"].(int)
			})

			start := (page - 1) * perPage
			end := start + perPage
			paged := make([]map[string]any, 0)
			if start < len(allRepos) {
				if end > len(allRepos) {
					end = len(allRepos)
				}
				paged = allRepos[start:end]
			}

			extraHeaders := make(map[string]string)
			if end < len(allRepos) {
				host := r.Host
				if host == "" {
					host = "localhost"
				}
				nextURL := fmt.Sprintf("http://%s/api/v3/user/repos?per_page=%d&page=%d", host, perPage, page+1)
				extraHeaders["Link"] = fmt.Sprintf("<%s>; rel=\"next\"", nextURL)
			}
			sendJSON(w, http.StatusOK, paged, extraHeaders)
			return
		}
	}

	// Repos sub-resources: /repos/:owner/:repo/...
	if strings.HasPrefix(restPath, "/repos/") {
		parts := strings.Split(strings.TrimPrefix(restPath, "/repos/"), "/")
		if len(parts) >= 2 {
			owner := parts[0]
			repoName := parts[1]
			subPath := ""
			if len(parts) > 2 {
				subPath = strings.Join(parts[2:], "/")
			}

			if strings.ToLower(owner) != strings.ToLower(store.Viewer.Login) {
				sendJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"}, nil)
				return
			}

			repo := store.GetRepo(owner, repoName)
			if repo == nil {
				sendJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"}, nil)
				return
			}

			repo.Mu.Lock()
			defer repo.Mu.Unlock()

			// Fast issues handling
			if strings.HasPrefix(subPath, "issues") {

				if subPath == "issues" {
					if r.Method == http.MethodPost {
						parsed, err := readJSONBody(r)
						if err != nil {
							sendJSON(w, http.StatusBadRequest, map[string]string{"message": "Problems parsing JSON"}, nil)
							return
						}
						title, _ := parsed["title"].(string)
						body, _ := parsed["body"].(string)
						attachedLabels := make([]Label, 0)
						if rawLabels, ok := parsed["labels"].([]any); ok {
							for _, rl := range rawLabels {
								lName, _ := rl.(string)
								if lbl, ok := repo.Labels[lName]; ok {
									attachedLabels = append(attachedLabels, lbl)
								} else {
									sendJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Label " + lName + " does not exist"}, nil)
									return
								}
							}
						}

						repo.IssueSeq++
						num := repo.IssueSeq
						now := time.Now().UTC().Format(time.RFC3339)
						issue := &Issue{
							ID:        num,
							Number:    num,
							Title:     title,
							Body:      body,
							State:     "open",
							Labels:    attachedLabels,
							CreatedAt: now,
							UpdatedAt: now,
						}
						issue.CachedJSON = serializeIssue(issue)
						repo.Issues[num] = issue

						w.Header().Set("Content-Type", "application/json; charset=utf-8")
						w.Header().Set("Content-Length", strconv.Itoa(len(issue.CachedJSON)))
						w.WriteHeader(http.StatusCreated)
						w.Write(issue.CachedJSON)
						return
					}

					if r.Method == http.MethodGet {
						stateFilter := r.URL.Query().Get("state")
						if stateFilter == "" {
							stateFilter = "open"
						}
						sort := r.URL.Query().Get("sort")
						direction := r.URL.Query().Get("direction")
						if direction == "" {
							direction = "desc"
						}

						var list []*Issue
						for _, iss := range repo.Issues {
							if stateFilter == "all" || iss.State == stateFilter {
								list = append(list, iss)
							}
						}
						if sort == "created" {
							if direction == "asc" {
								// asc: smaller number first
								for i := 0; i < len(list); i++ {
									for j := i + 1; j < len(list); j++ {
										if list[i].Number > list[j].Number {
											list[i], list[j] = list[j], list[i]
										}
									}
								}
							} else {
								// desc: larger number first
								for i := 0; i < len(list); i++ {
									for j := i + 1; j < len(list); j++ {
										if list[i].Number < list[j].Number {
											list[i], list[j] = list[j], list[i]
										}
									}
								}
							}
						}
						out := make([]map[string]any, 0)
						for _, i := range list {
							lbls := i.Labels
							if lbls == nil {
								lbls = []Label{}
							}
							out = append(out, map[string]any{
								"id":     i.ID,
								"number": i.Number,
								"title":  i.Title,
								"body":   i.Body,
								"state":  i.State,
								"labels": lbls,
							})
						}
						sendJSON(w, http.StatusOK, out, nil)
						return
					}
				}

				if strings.HasPrefix(subPath, "issues/") {
					issuePart := strings.TrimPrefix(subPath, "issues/")
					slashIdx := strings.IndexByte(issuePart, '/')
					if slashIdx == -1 {
						num, _ := strconv.Atoi(issuePart)
						issue, ok := repo.Issues[num]
						if !ok {
							sendJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"}, nil)
							return
						}
						if r.Method == http.MethodGet {
							if len(issue.CachedJSON) == 0 {
								issue.CachedJSON = serializeIssue(issue)
							}
							w.Header().Set("Content-Type", "application/json; charset=utf-8")
							w.Header().Set("Content-Length", strconv.Itoa(len(issue.CachedJSON)))
							w.WriteHeader(http.StatusOK)
							w.Write(issue.CachedJSON)
							return
						}
						if r.Method == http.MethodPatch {
							parsed, err := readJSONBody(r)
							if err != nil {
								sendJSON(w, http.StatusBadRequest, map[string]string{"message": "Problems parsing JSON"}, nil)
								return
							}
							if s, ok := parsed["state"].(string); ok {
								issue.State = s
							}
							if t, ok := parsed["title"].(string); ok {
								issue.Title = t
							}
							if b, ok := parsed["body"].(string); ok {
								issue.Body = b
							}
							issue.CachedJSON = serializeIssue(issue)
							lbls := issue.Labels
							if lbls == nil {
								lbls = []Label{}
							}
							sendJSON(w, http.StatusOK, map[string]any{
								"id":     issue.ID,
								"number": issue.Number,
								"title":  issue.Title,
								"body":   issue.Body,
								"state":  issue.State,
								"labels": lbls,
							}, nil)
							return
						}
					} else if issuePart[slashIdx+1:] == "comments" {
						num, _ := strconv.Atoi(issuePart[:slashIdx])
						issue, ok := repo.Issues[num]
						if !ok {
							sendJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"}, nil)
							return
						}
						if r.Method == http.MethodPost {
							parsed, err := readJSONBody(r)
							if err != nil {
								sendJSON(w, http.StatusBadRequest, map[string]string{"message": "Problems parsing JSON"}, nil)
								return
							}
							repo.CommentsSeq++
							body, _ := parsed["body"].(string)
							c := Comment{
								ID:        repo.CommentsSeq,
								Body:      body,
								CreatedAt: time.Now().UTC().Format(time.RFC3339),
								User:      store.Viewer,
							}
							issue.Comments = append(issue.Comments, c)
							sendJSON(w, http.StatusCreated, map[string]any{"id": c.ID, "body": c.Body}, nil)
							return
						}
						if r.Method == http.MethodGet {
							out := make([]map[string]any, 0)
							for _, c := range issue.Comments {
								out = append(out, map[string]any{"id": c.ID, "body": c.Body})
							}
							sendJSON(w, http.StatusOK, out, nil)
							return
						}
					}
				}
			}

			// GET /repos/:o/:r
			if subPath == "" && r.Method == http.MethodGet {
				sendJSON(w, http.StatusOK, map[string]any{
					"id":             repo.ID,
					"node_id":        repo.NodeID,
					"name":           repo.Name,
					"full_name":      repo.Owner.Login + "/" + repo.Name,
					"private":        repo.Private,
					"owner":          repo.Owner,
					"description":    repo.Description,
					"default_branch": repo.DefaultBranch,
				}, nil)
				return
			}

			// PATCH /repos/:o/:r
			if subPath == "" && r.Method == http.MethodPatch {
				parsed, err := readJSONBody(r)
				if err != nil {
					sendJSON(w, http.StatusBadRequest, map[string]string{"message": "Problems parsing JSON"}, nil)
					return
				}
				if d, ok := parsed["description"].(string); ok {
					repo.Description = &d
				} else if _, ok := parsed["description"]; ok {
					repo.Description = nil
				}
				sendJSON(w, http.StatusOK, map[string]any{
					"id":             repo.ID,
					"node_id":        repo.NodeID,
					"name":           repo.Name,
					"full_name":      repo.Owner.Login + "/" + repo.Name,
					"private":        repo.Private,
					"owner":          repo.Owner,
					"description":    repo.Description,
					"default_branch": repo.DefaultBranch,
				}, nil)
				return
			}

			// DELETE /repos/:o/:r
			if subPath == "" && r.Method == http.MethodDelete {
				store.DeleteRepo(owner, repoName)
				w.Header().Set("X-GitHub-Request-Id", "del-req")
				w.WriteHeader(http.StatusNoContent)
				return
			}

			// POST /repos/:o/:r/git/refs
			if subPath == "git/refs" && r.Method == http.MethodPost {
				parsed, err := readJSONBody(r)
				if err != nil {
					sendJSON(w, http.StatusBadRequest, map[string]string{"message": "Problems parsing JSON"}, nil)
					return
				}
				ref, _ := parsed["ref"].(string)
				sha, _ := parsed["sha"].(string)
				repo.Refs[ref] = sha
				host := r.Host
				if host == "" {
					host = "localhost"
				}
				sendJSON(w, http.StatusCreated, map[string]any{
					"ref":     ref,
					"node_id": base64.StdEncoding.EncodeToString([]byte("ref:" + ref)),
					"url":     fmt.Sprintf("http://%s/api/v3/repos/%s/%s/git/refs/%s", host, owner, repoName, ref),
					"object": map[string]any{
						"sha":  sha,
						"type": "commit",
						"url":  fmt.Sprintf("http://%s/api/v3/repos/%s/%s/git/commits/%s", host, owner, repoName, sha),
					},
				}, nil)
				return
			}

			// GET /repos/:o/:r/branches
			if subPath == "branches" && r.Method == http.MethodGet {
				host := r.Host
				if host == "" {
					host = "localhost"
				}
				branches := make([]map[string]any, 0)
				hasDef := false
				for refName, oid := range repo.Refs {
					if strings.HasPrefix(refName, "refs/heads/") {
						bName := strings.TrimPrefix(refName, "refs/heads/")
						if bName == repo.DefaultBranch {
							hasDef = true
						}
						branches = append(branches, map[string]any{
							"name": bName,
							"commit": map[string]any{
								"sha": oid,
								"url": fmt.Sprintf("http://%s/api/v3/repos/%s/%s/commits/%s", host, owner, repoName, oid),
							},
							"protected": false,
						})
					}
				}
				if !hasDef {
					if defOid, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
						branches = append(branches, map[string]any{
							"name": repo.DefaultBranch,
							"commit": map[string]any{
								"sha": defOid,
								"url": fmt.Sprintf("http://%s/api/v3/repos/%s/%s/commits/%s", host, owner, repoName, defOid),
							},
							"protected": false,
						})
					}
				}
				sendJSON(w, http.StatusOK, branches, nil)
				return
			}

			// GET /repos/:o/:r/commits
			if subPath == "commits" && r.Method == http.MethodGet {
				targetRef := r.URL.Query().Get("sha")
				if targetRef == "" {
					targetRef = repo.DefaultBranch
				}
				tip := repo.Refs["refs/heads/"+targetRef]
				if tip == "" {
					tip = repo.Refs[targetRef]
				}
				if tip == "" {
					tip = targetRef
				}

				commits := make([]map[string]any, 0)
				if tip != "" {
					queue := []string{tip}
					seen := make(map[string]bool)
					now := time.Now().UTC().Format(time.RFC3339)
					for len(queue) > 0 {
						oid := queue[0]
						queue = queue[1:]
						if seen[oid] {
							continue
						}
						seen[oid] = true
						obj, ok := repo.Objects[oid]
						if ok && obj.Type == TypeCommit {
							cv, err := ParseCommit(obj.Data)
							if err == nil {
								commits = append(commits, map[string]any{
									"sha": oid,
									"commit": map[string]any{
										"message": strings.TrimSpace(cv.Message),
										"author": map[string]any{
											"name":  store.Viewer.Name,
											"email": store.Viewer.Email,
											"date":  now,
										},
									},
								})
								queue = append(queue, cv.Parents...)
							}
						}
					}
				}
				sendJSON(w, http.StatusOK, commits, nil)
				return
			}

			// Contents API: /repos/:o/:r/contents/:path
			if strings.HasPrefix(subPath, "contents") {
				rawPath := strings.TrimPrefix(subPath, "contents")
				filePath := strings.Trim(rawPath, "/")

				if r.Method == http.MethodPut {
					parsed, err := readJSONBody(r)
					if err != nil {
						sendJSON(w, http.StatusBadRequest, map[string]string{"message": "Problems parsing JSON"}, nil)
						return
					}
					msg, _ := parsed["message"].(string)
					if msg == "" {
						msg = "Update content"
					}
					contentB64, _ := parsed["content"].(string)
					branch, _ := parsed["branch"].(string)
					if branch == "" {
						branch = repo.DefaultBranch
					}
					expectedSha, _ := parsed["sha"].(string)

					decoded, _ := base64.StdEncoding.DecodeString(contentB64)
					blobOid := repo.PutObject(TypeBlob, decoded)

					branchRef := "refs/heads/" + branch
					parentSha := repo.Refs[branchRef]
					var curTreeOid string
					if parentSha != "" {
						pObj, ok := repo.Objects[parentSha]
						if ok && pObj.Type == TypeCommit {
							cv, _ := ParseCommit(pObj.Data)
							curTreeOid = cv.Tree
						}
					}

					existingEntry := repo.FindEntryInTree(curTreeOid, filePath)
					if expectedSha != "" && curTreeOid != "" {
						if existingEntry != nil && existingEntry.OID != expectedSha {
							sendJSON(w, http.StatusConflict, map[string]string{"message": "Conflict: stale sha"}, nil)
							return
						}
					}

					newTreeOid := repo.UpdateTreeWithFile(curTreeOid, filePath, &blobOid)
					var parents []string
					if parentSha != "" {
						parents = []string{parentSha}
					}
					newCommitSha := repo.CreateCommit(newTreeOid, parents, msg)
					repo.Refs[branchRef] = newCommitSha

					status := http.StatusCreated
					if existingEntry != nil {
						status = http.StatusOK
					}
					parts := strings.Split(filePath, "/")
					fileName := parts[len(parts)-1]
					sendJSON(w, status, map[string]any{
						"content": map[string]any{
							"name":     fileName,
							"path":     filePath,
							"sha":      blobOid,
							"size":     len(decoded),
							"type":     "file",
							"encoding": "base64",
						},
						"commit": map[string]any{"sha": newCommitSha},
					}, nil)
					return
				}

				if r.Method == http.MethodGet {
					branch := r.URL.Query().Get("ref")
					if branch == "" {
						branch = repo.DefaultBranch
					}
					commitSha := repo.Refs["refs/heads/"+branch]
					if commitSha == "" {
						commitSha = repo.Refs[branch]
					}
					if commitSha == "" && repo.Objects[branch].Type == TypeCommit {
						commitSha = branch
					}
					if commitSha == "" {
						sendJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"}, nil)
						return
					}
					commitObj := repo.Objects[commitSha]
					if commitObj.Type != TypeCommit {
						sendJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"}, nil)
						return
					}
					cv, _ := ParseCommit(commitObj.Data)
					entry := repo.FindEntryInTree(cv.Tree, filePath)
					if entry == nil {
						sendJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"}, nil)
						return
					}

					if entry.Mode == "40000" {
						children := repo.GetTreeEntries(entry.OID)
						var out []map[string]any
						for _, c := range children {
							cPath := c.Name
							if filePath != "" {
								cPath = filePath + "/" + c.Name
							}
							cType := "file"
							if c.Mode == "40000" {
								cType = "dir"
							}
							out = append(out, map[string]any{
								"name": c.Name,
								"path": cPath,
								"sha":  c.OID,
								"type": cType,
								"size": 0,
							})
						}
						sendJSON(w, http.StatusOK, out, nil)
						return
					}

					blobObj := repo.Objects[entry.OID]
					parts := strings.Split(filePath, "/")
					fileName := parts[len(parts)-1]
					sendJSON(w, http.StatusOK, map[string]any{
						"type":     "file",
						"encoding": "base64",
						"content":  base64.StdEncoding.EncodeToString(blobObj.Data),
						"sha":      entry.OID,
						"size":     len(blobObj.Data),
						"name":     fileName,
						"path":     filePath,
					}, nil)
					return
				}

				if r.Method == http.MethodDelete {
					parsed, err := readJSONBody(r)
					if err != nil {
						sendJSON(w, http.StatusBadRequest, map[string]string{"message": "Problems parsing JSON"}, nil)
						return
					}
					msg, _ := parsed["message"].(string)
					if msg == "" {
						msg = "Delete content"
					}
					expectedSha, _ := parsed["sha"].(string)
					branch, _ := parsed["branch"].(string)
					if branch == "" {
						branch = repo.DefaultBranch
					}

					branchRef := "refs/heads/" + branch
					parentSha := repo.Refs[branchRef]
					if parentSha == "" {
						sendJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"}, nil)
						return
					}
					pObj := repo.Objects[parentSha]
					if pObj.Type != TypeCommit {
						sendJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"}, nil)
						return
					}
					cv, _ := ParseCommit(pObj.Data)
					existingEntry := repo.FindEntryInTree(cv.Tree, filePath)
					if existingEntry == nil || existingEntry.Mode == "40000" {
						sendJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"}, nil)
						return
					}
					if expectedSha != "" && existingEntry.OID != expectedSha {
						sendJSON(w, http.StatusConflict, map[string]string{"message": "Conflict: stale sha"}, nil)
						return
					}

					newTreeOid := repo.UpdateTreeWithFile(cv.Tree, filePath, nil)
					newCommitSha := repo.CreateCommit(newTreeOid, []string{parentSha}, msg)
					repo.Refs[branchRef] = newCommitSha

					sendJSON(w, http.StatusOK, map[string]any{
						"content": nil,
						"commit":  map[string]any{"sha": newCommitSha},
					}, nil)
					return
				}
			}

			// Labels API: POST /repos/:o/:r/labels
			if subPath == "labels" && r.Method == http.MethodPost {
				parsed, err := readJSONBody(r)
				if err != nil {
					sendJSON(w, http.StatusBadRequest, map[string]string{"message": "Problems parsing JSON"}, nil)
					return
				}
				name, _ := parsed["name"].(string)
				color, _ := parsed["color"].(string)
				if color == "" {
					color = "ededed"
				}
				label := Label{ID: len(repo.Labels) + 1, Name: name, Color: color}
				repo.Labels[name] = label
				sendJSON(w, http.StatusCreated, label, nil)
				return
			}

			// Pulls API: /repos/:o/:r/pulls...
			if subPath == "pulls" {
				if r.Method == http.MethodPost {
					parsed, err := readJSONBody(r)
					if err != nil {
						sendJSON(w, http.StatusBadRequest, map[string]string{"message": "Problems parsing JSON"}, nil)
						return
					}
					title, _ := parsed["title"].(string)
					body, _ := parsed["body"].(string)
					head, _ := parsed["head"].(string)
					base, _ := parsed["base"].(string)

					headSha := repo.Refs["refs/heads/"+head]
					if headSha == "" {
						headSha = ZeroOID
					}
					baseSha := repo.Refs["refs/heads/"+base]
					if baseSha == "" {
						baseSha = ZeroOID
					}

					repo.PullSeq++
					num := repo.PullSeq
					pull := &PullRequest{
						ID:            num,
						Number:        num,
						Title:         title,
						Body:          body,
						State:         "open",
						Merged:        false,
						Head:          HeadBase{Ref: head, SHA: headSha},
						Base:          HeadBase{Ref: base, SHA: baseSha},
						HeadCommitSHA: headSha,
						BaseCommitSHA: baseSha,
					}
					repo.Pulls[num] = pull
					sendJSON(w, http.StatusCreated, map[string]any{
						"number": pull.Number,
						"state":  pull.State,
						"title":  pull.Title,
						"body":   pull.Body,
						"head":   pull.Head,
						"base":   pull.Base,
					}, nil)
					return
				}

				if r.Method == http.MethodGet {
					stateFilter := r.URL.Query().Get("state")
					if stateFilter == "" {
						stateFilter = "open"
					}
					list := make([]map[string]any, 0)
					for _, p := range repo.Pulls {
						if stateFilter == "all" || p.State == stateFilter {
							list = append(list, map[string]any{
								"number": p.Number,
								"state":  p.State,
								"title":  p.Title,
								"body":   p.Body,
								"head":   p.Head,
								"base":   p.Base,
							})
						}
					}
					sendJSON(w, http.StatusOK, list, nil)
					return
				}
			}

			// Pulls detail: /repos/:o/:r/pulls/:number
			if strings.HasPrefix(subPath, "pulls/") {
				pullPart := strings.TrimPrefix(subPath, "pulls/")
				if !strings.Contains(pullPart, "/") {
					num, _ := strconv.Atoi(pullPart)
					pull, ok := repo.Pulls[num]
					if !ok {
						sendJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"}, nil)
						return
					}
					sendJSON(w, http.StatusOK, map[string]any{
						"number":           pull.Number,
						"state":            pull.State,
						"title":            pull.Title,
						"body":             pull.Body,
						"head":             pull.Head,
						"base":             pull.Base,
						"merged":           pull.Merged,
						"merged_at":        pull.MergedAt,
						"merge_commit_sha": pull.MergeCommitSHA,
					}, nil)
					return
				}

				if strings.HasSuffix(pullPart, "/files") && r.Method == http.MethodGet {
					numStr := strings.TrimSuffix(pullPart, "/files")
					num, _ := strconv.Atoi(numStr)
					pull, ok := repo.Pulls[num]
					if !ok {
						sendJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"}, nil)
						return
					}
					var headTreeOid, baseTreeOid string
					if hObj, ok := repo.Objects[pull.Head.SHA]; ok && hObj.Type == TypeCommit {
						cv, _ := ParseCommit(hObj.Data)
						headTreeOid = cv.Tree
					}
					if bObj, ok := repo.Objects[pull.Base.SHA]; ok && bObj.Type == TypeCommit {
						cv, _ := ParseCommit(bObj.Data)
						baseTreeOid = cv.Tree
					}
					var headFiles, baseFiles []TreeFile
					if headTreeOid != "" {
						headFiles = repo.ListTreeRecursive(headTreeOid, "")
					}
					if baseTreeOid != "" {
						baseFiles = repo.ListTreeRecursive(baseTreeOid, "")
					}
					baseMap := make(map[string]string)
					for _, bf := range baseFiles {
						baseMap[bf.Path] = bf.OID
					}
					changed := make([]map[string]any, 0)
					for _, hf := range headFiles {
						if hf.Type != "file" {
							continue
						}
						bOid, exists := baseMap[hf.Path]
						if !exists {
							changed = append(changed, map[string]any{
								"filename":  hf.Path,
								"status":    "added",
								"additions": 1,
								"deletions": 0,
								"changes":   1,
							})
						} else if bOid != hf.OID {
							changed = append(changed, map[string]any{
								"filename":  hf.Path,
								"status":    "modified",
								"additions": 1,
								"deletions": 0,
								"changes":   1,
							})
						}
					}
					sendJSON(w, http.StatusOK, changed, nil)
					return
				}

				if strings.HasSuffix(pullPart, "/merge") && r.Method == http.MethodPut {
					numStr := strings.TrimSuffix(pullPart, "/merge")
					num, _ := strconv.Atoi(numStr)
					pull, ok := repo.Pulls[num]
					if !ok {
						sendJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"}, nil)
						return
					}
					if pull.Merged {
						sendJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Pull request is not mergeable"}, nil)
						return
					}

					baseBranchRef := "refs/heads/" + pull.Base.Ref
					curBaseSha := repo.Refs[baseBranchRef]
					if curBaseSha == "" {
						curBaseSha = pull.Base.SHA
					}
					origBaseSha := pull.BaseCommitSHA
					if origBaseSha == "" {
						origBaseSha = pull.Base.SHA
					}
					headSha := pull.Head.SHA

					var headTreeOid, curBaseTreeOid, origBaseTreeOid string
					if hObj, ok := repo.Objects[headSha]; ok && hObj.Type == TypeCommit {
						cv, _ := ParseCommit(hObj.Data)
						headTreeOid = cv.Tree
					}
					if cObj, ok := repo.Objects[curBaseSha]; ok && cObj.Type == TypeCommit {
						cv, _ := ParseCommit(cObj.Data)
						curBaseTreeOid = cv.Tree
					}
					if oObj, ok := repo.Objects[origBaseSha]; ok && oObj.Type == TypeCommit {
						cv, _ := ParseCommit(oObj.Data)
						origBaseTreeOid = cv.Tree
					}

					var headFiles, curBaseFiles, origBaseFiles []TreeFile
					if headTreeOid != "" {
						headFiles = repo.ListTreeRecursive(headTreeOid, "")
					}
					if curBaseTreeOid != "" {
						curBaseFiles = repo.ListTreeRecursive(curBaseTreeOid, "")
					}
					if origBaseTreeOid != "" {
						origBaseFiles = repo.ListTreeRecursive(origBaseTreeOid, "")
					}

					headMap := make(map[string]string)
					for _, f := range headFiles {
						if f.Type == "file" {
							headMap[f.Path] = f.OID
						}
					}
					curBaseMap := make(map[string]string)
					for _, f := range curBaseFiles {
						if f.Type == "file" {
							curBaseMap[f.Path] = f.OID
						}
					}
					origBaseMap := make(map[string]string)
					for _, f := range origBaseFiles {
						if f.Type == "file" {
							origBaseMap[f.Path] = f.OID
						}
					}

					allPaths := make(map[string]bool)
					for p := range headMap {
						allPaths[p] = true
					}
					for p := range curBaseMap {
						allPaths[p] = true
					}
					for p := range origBaseMap {
						allPaths[p] = true
					}

					hasConflict := false
					for p := range allPaths {
						h := headMap[p]
						cur := curBaseMap[p]
						o := origBaseMap[p]
						if cur != o && h != o && cur != h {
							hasConflict = true
							break
						}
					}

					if hasConflict {
						sendJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Pull request is not mergeable"}, nil)
						return
					}

					mergedTreeOid := curBaseTreeOid
					if mergedTreeOid == "" {
						mergedTreeOid = headTreeOid
					}
					for p := range allPaths {
						h, hasH := headMap[p]
						o := origBaseMap[p]
						if h != o {
							if hasH {
								mergedTreeOid = repo.UpdateTreeWithFile(mergedTreeOid, p, &h)
							} else {
								mergedTreeOid = repo.UpdateTreeWithFile(mergedTreeOid, p, nil)
							}
						}
					}

					squashSha := repo.CreateCommit(mergedTreeOid, []string{curBaseSha}, fmt.Sprintf("Merge pull request #%d from %s", num, pull.Head.Ref))
					repo.Refs[baseBranchRef] = squashSha
					pull.State = "closed"
					pull.Merged = true
					now := time.Now().UTC().Format(time.RFC3339)
					pull.MergedAt = &now
					pull.MergeCommitSHA = &squashSha

					sendJSON(w, http.StatusOK, map[string]any{
						"sha":     squashSha,
						"merged":  true,
						"message": "Pull Request successfully merged",
					}, nil)
					return
				}
			}
		}
	}

	sendJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"}, nil)
}
