// Package state keeps the tool's local, git-ignored state under
// .agents/traits/.cache: the answer cache and (later) the findings log.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/talbet/traits/internal/provider"
)

// Cache is a content-addressed answer cache: one small JSON file per key.
// Keys are hex digests computed by the caller and already cover everything
// that affects the answer, so entries never need invalidating; stale ones
// are simply never read again. Errors are ignored: a cache miss is always
// safe.
type Cache struct {
	dir string
}

// OpenCache returns a cache rooted at dir, creating it and a .gitignore that
// keeps the whole cache directory out of version control.
func OpenCache(dir string) (*Cache, error) {
	answers := filepath.Join(dir, "answers")
	if err := os.MkdirAll(answers, 0o755); err != nil {
		return nil, err
	}
	ignore := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(ignore); os.IsNotExist(err) {
		_ = os.WriteFile(ignore, []byte("*\n"), 0o644)
	}
	return &Cache{dir: answers}, nil
}

func (c *Cache) path(key string) string {
	if len(key) < 3 {
		return filepath.Join(c.dir, key+".json")
	}
	return filepath.Join(c.dir, key[:2], key[2:]+".json")
}

// Get returns the cached answer for key.
func (c *Cache) Get(key string) (provider.Answer, bool) {
	data, err := os.ReadFile(c.path(key))
	if err != nil {
		return provider.Answer{}, false
	}
	var a provider.Answer
	if json.Unmarshal(data, &a) != nil {
		return provider.Answer{}, false
	}
	return a, true
}

// Put stores a under key. It writes to a temporary file and renames it so a
// concurrent reader never sees a partial entry.
func (c *Cache) Put(key string, a provider.Answer) {
	p := c.path(key)
	if os.MkdirAll(filepath.Dir(p), 0o755) != nil {
		return
	}
	data, err := json.Marshal(a)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), p) != nil {
		_ = os.Remove(tmp.Name())
	}
}
