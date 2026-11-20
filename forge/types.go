package main

import (
	"sync"
)

type User struct {
	ID    int    `json:"id"`
	Login string `json:"login"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type Label struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

type Comment struct {
	ID        int    `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	User      User   `json:"user"`
}

type Issue struct {
	ID         int       `json:"id"`
	Number     int       `json:"number"`
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	State      string    `json:"state"`
	Labels     []Label   `json:"labels"`
	CreatedAt  string    `json:"created_at"`
	UpdatedAt  string    `json:"updated_at"`
	Comments   []Comment `json:"-"`
	CachedJSON []byte    `json:"-"`
}

type HeadBase struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type PullRequest struct {
	ID             int       `json:"id"`
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	Body           string    `json:"body"`
	State          string    `json:"state"`
	Merged         bool      `json:"merged"`
	MergedAt       *string   `json:"merged_at"`
	MergeCommitSHA *string   `json:"merge_commit_sha"`
	Head           HeadBase  `json:"head"`
	Base           HeadBase  `json:"base"`
	HeadCommitSHA  string    `json:"headCommitSha"`
	BaseCommitSHA  string    `json:"baseCommitSha"`
}

type Repo struct {
	ID               int
	NodeID           string
	Name             string
	Owner            User
	Private          bool
	Description      *string
	DefaultBranch    string
	CreatedAt        string
	UpdatedAt        string
	Refs             map[string]string // ref name -> oid
	Objects          map[string]GitObject // oid -> GitObject
	Labels           map[string]Label
	Issues           map[int]*Issue
	IssueSeq         int
	Pulls            map[int]*PullRequest
	PullSeq          int
	CommentsSeq      int
	CommitParents    map[string][]string
	TreeEntries      map[string][]TreeEntry
	ShallowPackCache map[string][]byte
	LsRefsCache      map[string][]byte
	Mu               sync.RWMutex
}

func (r *Repo) InvalidateCache() {
	r.ShallowPackCache = make(map[string][]byte)
	r.LsRefsCache = make(map[string][]byte)
}
