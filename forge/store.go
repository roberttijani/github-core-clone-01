package main

import (
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"
)

type TreeFile struct {
	Path string
	OID  string
	Mode string
	Type string // "file" or "dir"
}

type ForgeStore struct {
	Viewer     User
	NextRepoID int
	Repos      map[string]*Repo // "owner/name" -> Repo
	Mu         sync.RWMutex
}

func NewForgeStore(login string) *ForgeStore {
	if login == "" {
		login = "adaiversen"
	}
	return &ForgeStore{
		Viewer: User{
			ID:    1000,
			Login: login,
			Name:  "ada iversen",
			Email: "adaiversen@example.invalid",
		},
		NextRepoID: 1,
		Repos:      make(map[string]*Repo),
	}
}

func repoKey(owner, name string) string {
	return strings.ToLower(owner) + "/" + strings.ToLower(name)
}

func (s *ForgeStore) GetRepo(owner, name string) *Repo {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	return s.Repos[repoKey(owner, name)]
}

func (s *ForgeStore) CreateRepo(name string, isPrivate bool, description *string) *Repo {
	s.Mu.Lock()
	defer s.Mu.Unlock()

	id := s.NextRepoID
	s.NextRepoID++
	nodeID := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("010:Repository%d", id)))
	now := time.Now().UTC().Format(time.RFC3339)

	r := &Repo{
		ID:               id,
		NodeID:           nodeID,
		Name:             name,
		Owner:            s.Viewer,
		Private:          isPrivate,
		Description:      description,
		DefaultBranch:    "main",
		CreatedAt:        now,
		UpdatedAt:        now,
		Refs:             make(map[string]string),
		Objects:          make(map[string]GitObject),
		Labels:           make(map[string]Label),
		Issues:           make(map[int]*Issue),
		IssueSeq:         0,
		Pulls:            make(map[int]*PullRequest),
		PullSeq:          0,
		CommentsSeq:      0,
		CommitParents:    make(map[string][]string),
		TreeEntries:      make(map[string][]TreeEntry),
		ShallowPackCache: make(map[string][]byte),
		LsRefsCache:      make(map[string][]byte),
	}
	s.Repos[repoKey(s.Viewer.Login, name)] = r
	return r
}

func (s *ForgeStore) DeleteRepo(owner, name string) bool {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	key := repoKey(owner, name)
	if _, ok := s.Repos[key]; ok {
		delete(s.Repos, key)
		return true
	}
	return false
}

func (r *Repo) PutObject(objType GitObjectType, data []byte) string {
	oid := HashObject(objType, data)
	if _, ok := r.Objects[oid]; !ok {
		r.Objects[oid] = GitObject{Type: objType, Data: data, OID: oid}
	}
	return oid
}

func (r *Repo) GetTreeEntries(treeOid string) []TreeEntry {
	if entries, ok := r.TreeEntries[treeOid]; ok {
		return entries
	}
	obj, ok := r.Objects[treeOid]
	if !ok || obj.Type != TypeTree {
		return nil
	}
	entries, err := ParseTree(obj.Data)
	if err != nil {
		return nil
	}
	r.TreeEntries[treeOid] = entries
	return entries
}

func (r *Repo) FindEntryInTree(treeOid string, filePath string) *TreeEntry {
	rawSegs := strings.Split(filePath, "/")
	var segs []string
	for _, s := range rawSegs {
		if s != "" {
			segs = append(segs, s)
		}
	}
	if len(segs) == 0 {
		return &TreeEntry{Mode: "40000", OID: treeOid}
	}
	curOid := treeOid
	for i, seg := range segs {
		entries := r.GetTreeEntries(curOid)
		var match *TreeEntry
		for j := range entries {
			if entries[j].Name == seg {
				match = &entries[j]
				break
			}
		}
		if match == nil {
			return nil
		}
		if i == len(segs)-1 {
			return match
		}
		if match.Mode != "40000" {
			return nil
		}
		curOid = match.OID
	}
	return nil
}

func (r *Repo) ListTreeRecursive(treeOid string, prefix string) []TreeFile {
	var result []TreeFile
	entries := r.GetTreeEntries(treeOid)
	for _, entry := range entries {
		fullPath := entry.Name
		if prefix != "" {
			fullPath = prefix + "/" + entry.Name
		}
		if entry.Mode == "40000" {
			result = append(result, TreeFile{Path: fullPath, OID: entry.OID, Mode: entry.Mode, Type: "dir"})
			result = append(result, r.ListTreeRecursive(entry.OID, fullPath)...)
		} else {
			result = append(result, TreeFile{Path: fullPath, OID: entry.OID, Mode: entry.Mode, Type: "file"})
		}
	}
	return result
}

func (r *Repo) UpdateTreeWithFile(treeOid string, filePath string, blobOid *string) string {
	rawSegs := strings.Split(filePath, "/")
	var segs []string
	for _, s := range rawSegs {
		if s != "" {
			segs = append(segs, s)
		}
	}

	var updateLevel func(curTreeOid string, depth int) string
	updateLevel = func(curTreeOid string, depth int) string {
		seg := segs[depth]
		var entries []TreeEntry
		if curTreeOid != "" {
			entries = append([]TreeEntry(nil), r.GetTreeEntries(curTreeOid)...)
		}
		isLeaf := depth == len(segs)-1

		if isLeaf {
			idx := -1
			for i := range entries {
				if entries[i].Name == seg {
					idx = i
					break
				}
			}
			if blobOid == nil {
				if idx >= 0 {
					entries = append(entries[:idx], entries[idx+1:]...)
				}
			} else {
				newEntry := TreeEntry{Mode: "100644", Name: seg, OID: *blobOid}
				if idx >= 0 {
					entries[idx] = newEntry
				} else {
					entries = append(entries, newEntry)
				}
			}
		} else {
			idx := -1
			for i := range entries {
				if entries[i].Name == seg && entries[i].Mode == "40000" {
					idx = i
					break
				}
			}
			subTreeOid := ""
			if idx >= 0 {
				subTreeOid = entries[idx].OID
			}
			newSubTreeOid := updateLevel(subTreeOid, depth+1)
			subEntries := r.GetTreeEntries(newSubTreeOid)
			if len(subEntries) == 0 {
				if idx >= 0 {
					entries = append(entries[:idx], entries[idx+1:]...)
				}
			} else {
				newEntry := TreeEntry{Mode: "40000", Name: seg, OID: newSubTreeOid}
				if idx >= 0 {
					entries[idx] = newEntry
				} else {
					entries = append(entries, newEntry)
				}
			}
		}

		treeData := BuildTree(entries)
		oid := r.PutObject(TypeTree, treeData)
		r.TreeEntries[oid] = entries
		return oid
	}

	return updateLevel(treeOid, 0)
}

func (r *Repo) CreateCommit(treeOid string, parents []string, message string) string {
	data := BuildCommit(treeOid, parents, message, "")
	oid := r.PutObject(TypeCommit, data)
	r.CommitParents[oid] = parents
	return oid
}

func (r *Repo) CollectReachableOIDs(tips []string, stopOids map[string]bool) []string {
	seen := make(map[string]bool)
	var queue []string
	for _, t := range tips {
		if t != "" && t != ZeroOID {
			queue = append(queue, t)
		}
	}
	var reachable []string

	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if oid == "" || oid == ZeroOID || seen[oid] || (stopOids != nil && stopOids[oid]) {
			continue
		}
		seen[oid] = true
		reachable = append(reachable, oid)

		obj, ok := r.Objects[oid]
		if !ok {
			continue
		}
		if obj.Type == TypeCommit {
			parents, ok := r.CommitParents[oid]
			var treeOid string
			if !ok {
				cv, err := ParseCommit(obj.Data)
				if err == nil {
					parents = cv.Parents
					treeOid = cv.Tree
					r.CommitParents[oid] = parents
				}
			} else {
				cv, _ := ParseCommit(obj.Data)
				treeOid = cv.Tree
			}
			if treeOid != "" {
				queue = append(queue, treeOid)
			}
			for _, p := range parents {
				queue = append(queue, p)
			}
		} else if obj.Type == TypeTree {
			entries := r.GetTreeEntries(oid)
			for _, e := range entries {
				if e.Mode != "160000" { // skip gitlink
					queue = append(queue, e.OID)
				}
			}
		}
	}
	return reachable
}

func (r *Repo) IsFastForward(oldOid, newOid string) bool {
	if oldOid == newOid {
		return true
	}
	queue := []string{newOid}
	seen := make(map[string]bool)
	for len(queue) > 0 {
		cur := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if cur == oldOid {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		obj, ok := r.Objects[cur]
		if !ok || obj.Type != TypeCommit {
			continue
		}
		parents, ok := r.CommitParents[cur]
		if !ok {
			cv, err := ParseCommit(obj.Data)
			if err == nil {
				parents = cv.Parents
				r.CommitParents[cur] = parents
			}
		}
		for _, p := range parents {
			queue = append(queue, p)
		}
	}
	return false
}
