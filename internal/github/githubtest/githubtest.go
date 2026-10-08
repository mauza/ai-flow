// Package githubtest fakes the slice of the GitHub API ai-flow uses for
// repository linking and product workspaces.
package githubtest

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

func blobSHA(b []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(b))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// Repo is a tiny in-memory GitHub repository (owner/name, branch main):
// commits are file maps. It serves the git data API the workspace uses, the
// repository details and /user/repos.
type Repo struct {
	Owner, Name, Description string

	mu      sync.Mutex
	commits map[string]map[string]string // commit sha → path → content
	trees   map[string]map[string]string // tree sha → files
	head    string
	n       int
	authors []map[string]any
}

func New(owner, name string, files map[string]string) *Repo {
	r := &Repo{Owner: owner, Name: name, commits: map[string]map[string]string{}, trees: map[string]map[string]string{}}
	r.head = r.add(files)
	return r
}

func (r *Repo) add(files map[string]string) string {
	r.n++
	sha := fmt.Sprintf("c%039d", r.n)
	r.commits[sha] = files
	r.trees["t"+sha[1:]] = files
	return sha
}

// Push simulates someone else committing.
func (r *Repo) Push(change map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := map[string]string{}
	for k, v := range r.commits[r.head] {
		next[k] = v
	}
	for k, v := range change {
		if v == "" {
			delete(next, k)
		} else {
			next[k] = v
		}
	}
	r.head = r.add(next)
}

// Files returns the files at the branch head.
func (r *Repo) Files() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.commits[r.head]
}

// Head is the branch head commit.
func (r *Repo) Head() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.head
}

func (r *Repo) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	full := r.Owner + "/" + r.Name
	if req.URL.Path == "/user/repos" {
		json.NewEncoder(w).Encode([]map[string]any{{"full_name": full, "description": r.Description, "default_branch": "main", "pushed_at": "2026-10-08T00:00:00Z"}})
		return
	}
	if req.URL.Path == "/repos/"+full {
		json.NewEncoder(w).Encode(map[string]any{"full_name": full, "description": r.Description, "default_branch": "main"})
		return
	}
	p, ok := strings.CutPrefix(req.URL.Path, "/repos/"+full)
	if !ok {
		http.NotFound(w, req)
		return
	}
	switch {
	case req.Method == "GET" && p == "/branches/main":
		json.NewEncoder(w).Encode(map[string]any{"commit": map[string]any{"sha": r.head}})
	case req.Method == "GET" && strings.HasPrefix(p, "/git/trees/"):
		files := r.commits[strings.TrimPrefix(p, "/git/trees/")]
		var tree []map[string]any
		for path, content := range files {
			tree = append(tree, map[string]any{"path": path, "type": "blob", "sha": blobSHA([]byte(content))})
		}
		json.NewEncoder(w).Encode(map[string]any{"tree": tree})
	case req.Method == "GET" && strings.HasPrefix(p, "/git/blobs/"):
		sha := strings.TrimPrefix(p, "/git/blobs/")
		for _, files := range r.commits {
			for _, c := range files {
				if blobSHA([]byte(c)) == sha {
					json.NewEncoder(w).Encode(map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(c))})
					return
				}
			}
		}
		http.NotFound(w, req)
	case req.Method == "GET" && strings.HasPrefix(p, "/git/commits/"):
		sha := strings.TrimPrefix(p, "/git/commits/")
		json.NewEncoder(w).Encode(map[string]any{"tree": map[string]any{"sha": "t" + sha[1:]}})
	case req.Method == "POST" && p == "/git/trees":
		var in struct {
			BaseTree string           `json:"base_tree"`
			Tree     []map[string]any `json:"tree"`
		}
		json.NewDecoder(req.Body).Decode(&in)
		files := map[string]string{}
		for k, v := range r.trees[in.BaseTree] {
			files[k] = v
		}
		for _, e := range in.Tree {
			if c, ok := e["content"].(string); ok {
				files[e["path"].(string)] = c
			} else {
				delete(files, e["path"].(string))
			}
		}
		r.n++
		sha := fmt.Sprintf("t%039d", r.n)
		r.trees[sha] = files
		json.NewEncoder(w).Encode(map[string]any{"sha": sha})
	case req.Method == "POST" && p == "/git/commits":
		var in struct {
			Tree    string         `json:"tree"`
			Author  map[string]any `json:"author"`
			Parents []string       `json:"parents"`
		}
		json.NewDecoder(req.Body).Decode(&in)
		r.authors = append(r.authors, in.Author)
		r.n++
		sha := fmt.Sprintf("c%039d", r.n)
		r.commits[sha] = r.trees[in.Tree]
		r.trees["t"+sha[1:]] = r.trees[in.Tree]
		parent := in.Parents[0]
		r.commits[sha+"-parent"] = map[string]string{"": parent}
		json.NewEncoder(w).Encode(map[string]any{"sha": sha})
	case req.Method == "PATCH" && p == "/git/refs/heads/main":
		var in struct {
			SHA string `json:"sha"`
		}
		json.NewDecoder(req.Body).Decode(&in)
		if r.commits[in.SHA+"-parent"][""] != r.head {
			w.WriteHeader(422)
			w.Write([]byte(`{"message":"Update is not a fast forward"}`))
			return
		}
		r.head = in.SHA
		w.Write([]byte(`{}`))
	default:
		http.Error(w, "unexpected "+req.Method+" "+p, 500)
	}
}
