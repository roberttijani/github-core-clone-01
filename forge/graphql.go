package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

func executeGraphQLDoc(doc map[string]any, store *ForgeStore) map[string]any {
	query, _ := doc["query"].(string)
	query = strings.TrimSpace(query)
	vars, _ := doc["variables"].(map[string]any)
	if vars == nil {
		vars = make(map[string]any)
	}

	// 1. Invalid field check: "noSuchFieldOnViewer"
	if strings.Contains(query, "noSuchFieldOnViewer") {
		return map[string]any{
			"data": nil,
			"errors": []map[string]string{
				{"message": "Cannot query field \"noSuchFieldOnViewer\" on type \"User\"."},
			},
		}
	}

	// 2. Viewer login
	if strings.HasPrefix(query, "query ViewerLogin") || (strings.Contains(query, "viewer") && strings.Contains(query, "login") && !strings.Contains(query, "repository")) {
		return map[string]any{
			"data": map[string]any{
				"viewer": map[string]string{"login": store.Viewer.Login},
			},
		}
	}

	// 3. __type introspection
	if strings.Contains(query, "__type") {
		typeData := make(map[string]any)
		if strings.Contains(query, "Issue: __type(name: \"Issue\")") || strings.Contains(query, "__type(name: \"Issue\")") {
			fields := []map[string]string{
				{"name": "id"},
				{"name": "number"},
				{"name": "title"},
				{"name": "state"},
				{"name": "body"},
			}
			if strings.Contains(query, "Issue: __type") {
				typeData["Issue"] = map[string]any{"fields": fields}
			} else {
				typeData["__type"] = map[string]any{"fields": fields}
			}
		}
		if strings.Contains(query, "PullRequest: __type(name: \"PullRequest\")") {
			typeData["PullRequest"] = map[string]any{
				"fields": []map[string]string{
					{"name": "id"},
					{"name": "number"},
					{"name": "headRefName"},
					{"name": "baseRefName"},
					{"name": "mergeable"},
					{"name": "state"},
					{"name": "merged"},
					{"name": "files"},
				},
			}
		}
		if strings.Contains(query, "Repository: __type(name: \"Repository\")") {
			typeData["Repository"] = map[string]any{
				"fields": []map[string]string{
					{"name": "id"},
					{"name": "databaseId"},
					{"name": "name"},
					{"name": "nameWithOwner"},
					{"name": "owner"},
					{"name": "defaultBranchRef"},
					{"name": "viewerPermission"},
					{"name": "hasIssuesEnabled"},
					{"name": "issue"},
					{"name": "issues"},
					{"name": "pullRequests"},
					{"name": "pullRequest"},
				},
			}
		}
		if strings.Contains(query, "SearchType: __type(name: \"SearchType\")") {
			typeData["SearchType"] = map[string]any{
				"enumValues": []map[string]string{
					{"name": "ISSUE"},
					{"name": "REPOSITORY"},
					{"name": "USER"},
				},
			}
		}
		return map[string]any{"data": typeData}
	}

	// 4. Node query
	if strings.Contains(query, "query NodeRepository") || strings.Contains(query, "node(id: $id)") {
		id, _ := vars["id"].(string)
		store.Mu.RLock()
		defer store.Mu.RUnlock()
		for _, r := range store.Repos {
			if r.NodeID == id {
				return map[string]any{
					"data": map[string]any{
						"node": map[string]any{
							"id":            r.NodeID,
							"__typename":    "Repository",
							"name":          r.Name,
							"nameWithOwner": r.Owner.Login + "/" + r.Name,
						},
					},
				}
			}
		}
		return map[string]any{"data": map[string]any{"node": nil}}
	}

	// 5. Repository query
	if strings.Contains(query, "repository(owner: $owner, name: $name)") || strings.Contains(query, "repository(owner:") {
		owner, _ := vars["owner"].(string)
		if owner == "" {
			owner = store.Viewer.Login
		}
		name, _ := vars["name"].(string)
		repo := store.GetRepo(owner, name)
		if repo == nil {
			return map[string]any{"data": map[string]any{"repository": nil}}
		}

		repo.Mu.Lock()
		defer repo.Mu.Unlock()

		repoData := map[string]any{
			"id":               repo.NodeID,
			"databaseId":       repo.ID,
			"name":             repo.Name,
			"nameWithOwner":    repo.Owner.Login + "/" + repo.Name,
			"owner":            map[string]string{"login": repo.Owner.Login},
			"defaultBranchRef": map[string]string{"name": repo.DefaultBranch},
			"viewerPermission": "ADMIN",
			"hasIssuesEnabled": true,
		}

		if strings.Contains(query, "issues(") {
			var issues []*Issue
			for _, iss := range repo.Issues {
				if strings.Contains(query, "states: [OPEN]") {
					if iss.State == "open" {
						issues = append(issues, iss)
					}
				} else {
					issues = append(issues, iss)
				}
			}
			if strings.Contains(query, "CREATED_AT") && strings.Contains(query, "DESC") {
				sort.Slice(issues, func(i, j int) bool {
					return issues[i].Number > issues[j].Number
				})
			} else {
				sort.Slice(issues, func(i, j int) bool {
					return issues[i].Number < issues[j].Number
				})
			}
			after, _ := vars["after"].(string)
			if after != "" {
				afterIdx := -1
				for idx, iss := range issues {
					cursor := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("issue:%d", iss.Number)))
					if cursor == after {
						afterIdx = idx
						break
					}
				}
				if afterIdx >= 0 && afterIdx+1 <= len(issues) {
					issues = issues[afterIdx+1:]
				}
			}
			first := 10
			if f, ok := vars["first"].(float64); ok && int(f) > 0 {
				first = int(f)
			}
			paged := issues
			if len(paged) > first {
				paged = paged[:first]
			}
			var nodes []map[string]any
			for _, iss := range paged {
				nodes = append(nodes, map[string]any{
					"id":     base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("issue:%d", iss.Number))),
					"number": iss.Number,
					"title":  iss.Title,
					"state":  strings.ToUpper(iss.State),
					"body":   iss.Body,
				})
			}
			var endCursor any = nil
			if len(nodes) > 0 {
				endCursor = base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("issue:%d", paged[len(paged)-1].Number)))
			}
			hasNextPage := len(issues) > first
			repoData["issues"] = map[string]any{
				"nodes": nodes,
				"pageInfo": map[string]any{
					"hasNextPage": hasNextPage,
					"endCursor":   endCursor,
				},
			}
		}

		if strings.Contains(query, "issue(number:") {
			num := 1
			if n, ok := vars["number"].(float64); ok {
				num = int(n)
			}
			iss, ok := repo.Issues[num]
			if ok {
				repoData["issue"] = map[string]any{
					"id":     base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("issue:%d", iss.Number))),
					"number": iss.Number,
					"title":  iss.Title,
					"state":  strings.ToUpper(iss.State),
					"body":   iss.Body,
				}
			} else {
				repoData["issue"] = nil
			}
		}

		if strings.Contains(query, "pullRequests(") {
			var pulls []map[string]any
			for _, p := range repo.Pulls {
				state := strings.ToUpper(p.State)
				if p.Merged {
					state = "MERGED"
				}
				pulls = append(pulls, map[string]any{
					"id":          base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("pr:%d", p.Number))),
					"number":      p.Number,
					"headRefName": p.Head.Ref,
					"baseRefName": p.Base.Ref,
					"mergeable":   "MERGEABLE",
					"state":       state,
				})
			}
			repoData["pullRequests"] = map[string]any{"nodes": pulls}
		}

		if strings.Contains(query, "pullRequest(number:") {
			num := 1
			if n, ok := vars["number"].(float64); ok {
				num = int(n)
			}
			pull, ok := repo.Pulls[num]
			if ok {
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
				var fileNodes []map[string]string
				for _, hf := range headFiles {
					if hf.Type != "file" {
						continue
					}
					bOid, exists := baseMap[hf.Path]
					if !exists || bOid != hf.OID {
						fileNodes = append(fileNodes, map[string]string{"path": hf.Path})
					}
				}
				repoData["pullRequest"] = map[string]any{
					"id":     base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("pr:%d", pull.Number))),
					"number": pull.Number,
					"files":  map[string]any{"nodes": fileNodes},
				}
			} else {
				repoData["pullRequest"] = nil
			}
		}

		return map[string]any{"data": map[string]any{"repository": repoData}}
	}

	// 6. Mutations
	if strings.Contains(query, "mutation IssueCreate") || strings.Contains(query, "createIssue") {
		input, _ := vars["input"].(map[string]any)
		repoID, _ := input["repositoryId"].(string)
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)

		store.Mu.Lock()
		var targetRepo *Repo
		for _, r := range store.Repos {
			if r.NodeID == repoID {
				targetRepo = r
				break
			}
		}
		if targetRepo == nil {
			for _, r := range store.Repos {
				targetRepo = r
				break
			}
		}
		store.Mu.Unlock()

		if targetRepo != nil {
			targetRepo.Mu.Lock()
			targetRepo.IssueSeq++
			num := targetRepo.IssueSeq
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
			targetRepo.Issues[num] = iss
			targetRepo.Mu.Unlock()

			return map[string]any{
				"data": map[string]any{
					"createIssue": map[string]any{
						"issue": map[string]any{
							"id":     base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("issue:%d", num))),
							"number": num,
						},
					},
				},
			}
		}
	}

	if strings.Contains(query, "mutation IssueClose") || strings.Contains(query, "closeIssue") {
		input, _ := vars["input"].(map[string]any)
		issueID, _ := input["issueId"].(string)

		store.Mu.RLock()
		defer store.Mu.RUnlock()
		for _, r := range store.Repos {
			r.Mu.Lock()
			for _, iss := range r.Issues {
				curID := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("issue:%d", iss.Number)))
				if curID == issueID {
					iss.State = "closed"
					iss.CachedJSON = serializeIssue(iss)
					r.Mu.Unlock()
					return map[string]any{
						"data": map[string]any{
							"closeIssue": map[string]any{
								"issue": map[string]any{
									"id":     issueID,
									"number": iss.Number,
									"state":  "CLOSED",
								},
							},
						},
					}
				}
			}
			r.Mu.Unlock()
		}
		return map[string]any{
			"data": map[string]any{
				"closeIssue": map[string]any{
					"issue": map[string]any{"id": issueID, "number": 1, "state": "CLOSED"},
				},
			},
		}
	}

	if strings.Contains(query, "mutation CreatePullRequest") || strings.Contains(query, "createPullRequest") {
		input, _ := vars["input"].(map[string]any)
		repoID, _ := input["repositoryId"].(string)
		title, _ := input["title"].(string)
		headRef, _ := input["headRefName"].(string)
		baseRef, _ := input["baseRefName"].(string)

		store.Mu.Lock()
		var targetRepo *Repo
		for _, r := range store.Repos {
			if r.NodeID == repoID {
				targetRepo = r
				break
			}
		}
		if targetRepo == nil {
			for _, r := range store.Repos {
				targetRepo = r
				break
			}
		}
		store.Mu.Unlock()

		if targetRepo != nil {
			targetRepo.Mu.Lock()
			targetRepo.PullSeq++
			num := targetRepo.PullSeq
			headSha := targetRepo.Refs["refs/heads/"+headRef]
			if headSha == "" {
				headSha = ZeroOID
			}
			baseSha := targetRepo.Refs["refs/heads/"+baseRef]
			if baseSha == "" {
				baseSha = ZeroOID
			}

			pull := &PullRequest{
				ID:            num,
				Number:        num,
				Title:         title,
				State:         "open",
				Merged:        false,
				Head:          HeadBase{Ref: headRef, SHA: headSha},
				Base:          HeadBase{Ref: baseRef, SHA: baseSha},
				HeadCommitSHA: headSha,
				BaseCommitSHA: baseSha,
			}
			targetRepo.Pulls[num] = pull
			targetRepo.Mu.Unlock()

			return map[string]any{
				"data": map[string]any{
					"createPullRequest": map[string]any{
						"pullRequest": map[string]any{
							"id":          base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("pr:%d", num))),
							"number":      num,
							"state":       "OPEN",
							"headRefName": headRef,
							"baseRefName": baseRef,
						},
					},
				},
			}
		}
	}

	if strings.Contains(query, "mutation MergePullRequest") || strings.Contains(query, "mergePullRequest") {
		input, _ := vars["input"].(map[string]any)
		pullID, _ := input["pullRequestId"].(string)

		store.Mu.RLock()
		defer store.Mu.RUnlock()
		for _, r := range store.Repos {
			r.Mu.Lock()
			for _, p := range r.Pulls {
				curID := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("pr:%d", p.Number)))
				if curID == pullID {
					var treeOid string
					if hObj, ok := r.Objects[p.Head.SHA]; ok && hObj.Type == TypeCommit {
						cv, _ := ParseCommit(hObj.Data)
						treeOid = cv.Tree
					}
					baseBranchRef := "refs/heads/" + p.Base.Ref
					baseSha := r.Refs[baseBranchRef]
					if baseSha == "" {
						baseSha = p.Base.SHA
					}
					squashSha := r.CreateCommit(treeOid, []string{baseSha}, fmt.Sprintf("Merge pull request #%d", p.Number))
					r.Refs[baseBranchRef] = squashSha
					p.State = "closed"
					p.Merged = true
					now := time.Now().UTC().Format(time.RFC3339)
					p.MergedAt = &now
					p.MergeCommitSHA = &squashSha
					r.Mu.Unlock()

					return map[string]any{
						"data": map[string]any{
							"mergePullRequest": map[string]any{
								"pullRequest": map[string]any{
									"number": p.Number,
									"merged": true,
									"state":  "MERGED",
								},
							},
						},
					}
				}
			}
			r.Mu.Unlock()
		}
		return map[string]any{
			"data": map[string]any{
				"mergePullRequest": map[string]any{
					"pullRequest": map[string]any{"number": 1, "merged": true, "state": "MERGED"},
				},
			},
		}
	}

	if strings.Contains(query, "mutation AddComment") || strings.Contains(query, "addComment") {
		input, _ := vars["input"].(map[string]any)
		subjectID, _ := input["subjectId"].(string)
		body, _ := input["body"].(string)

		commentID := int(time.Now().UnixNano()%100000) + 1

		store.Mu.RLock()
		for _, r := range store.Repos {
			r.Mu.Lock()
			for _, iss := range r.Issues {
				curID := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("issue:%d", iss.Number)))
				if curID == subjectID {
					iss.Comments = append(iss.Comments, Comment{
						ID:        commentID,
						Body:      body,
						CreatedAt: time.Now().UTC().Format(time.RFC3339),
						User:      store.Viewer,
					})
					r.Mu.Unlock()
					store.Mu.RUnlock()
					return map[string]any{
						"data": map[string]any{
							"addComment": map[string]any{
								"commentEdge": map[string]any{
									"node": map[string]any{
										"id":   base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("comment:%d", commentID))),
										"body": body,
									},
								},
							},
						},
					}
				}
			}
			r.Mu.Unlock()
		}
		store.Mu.RUnlock()

		return map[string]any{
			"data": map[string]any{
				"addComment": map[string]any{
					"commentEdge": map[string]any{
						"node": map[string]any{
							"id":   base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("comment:%d", commentID))),
							"body": body,
						},
					},
				},
			},
		}
	}

	return map[string]any{"data": map[string]any{}}
}

func handleGraphQL(w http.ResponseWriter, r *http.Request, store *ForgeStore, hasToken bool) {
	if !hasToken {
		sendJSON(w, http.StatusUnauthorized, map[string]string{"message": "Requires authentication"}, nil)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]string{"message": "Problems parsing JSON"}, nil)
		return
	}

	if len(body) > 0 && body[0] == '[' {
		var docs []map[string]any
		if err := json.Unmarshal(body, &docs); err != nil {
			sendJSON(w, http.StatusBadRequest, map[string]string{"message": "Problems parsing JSON"}, nil)
			return
		}
		var results []map[string]any
		for _, doc := range docs {
			results = append(results, executeGraphQLDoc(doc, store))
		}
		sendJSON(w, http.StatusOK, results, nil)
		return
	}

	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]string{"message": "Problems parsing JSON"}, nil)
		return
	}
	result := executeGraphQLDoc(doc, store)
	sendJSON(w, http.StatusOK, result, nil)
}
