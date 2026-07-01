package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Store keeps an in-memory HAR log per session and rewrites the backing file
// atomically on every new entry. A single mutex serializes read-modify-write so
// concurrent requests never corrupt a file.
type Store struct {
	dir    string
	pretty bool

	mu   sync.Mutex
	logs map[string]*HarLog
}

func NewStore(dir string, pretty bool) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir, pretty: pretty, logs: map[string]*HarLog{}}, nil
}

func (s *Store) Add(session string, e HarEntry) error {
	key := sanitize(session)

	s.mu.Lock()
	defer s.mu.Unlock()

	log, ok := s.logs[key]
	if !ok {
		log = s.load(key)
		s.logs[key] = log
	}
	log.Entries = append(log.Entries, e)
	// Keep the page's start no later than its first entry.
	if len(log.Entries) == 1 && len(log.Pages) > 0 {
		log.Pages[0].StartedDateTime = e.StartedDateTime
	}
	return s.write(key, log)
}

// load reads an existing .har for the key so re-runs append to it, or returns a
// fresh log.
func (s *Store) load(key string) *HarLog {
	if data, err := os.ReadFile(s.path(key)); err == nil {
		var h HAR
		if json.Unmarshal(data, &h) == nil && h.Log.Version != "" {
			if h.Log.Entries == nil {
				h.Log.Entries = []HarEntry{}
			}
			if len(h.Log.Pages) == 0 {
				h.Log.Pages = []HarPage{newPage()}
			}
			return &h.Log
		}
	}
	return newHarLog()
}

// write serializes the log and swaps it in atomically via a temp file + rename.
func (s *Store) write(key string, log *HarLog) error {
	var (
		data []byte
		err  error
	)
	h := HAR{Log: *log}
	if s.pretty {
		data, err = json.MarshalIndent(h, "", "  ")
	} else {
		data, err = json.Marshal(h)
	}
	if err != nil {
		return err
	}

	path := s.path(key)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Store) path(key string) string {
	return filepath.Join(s.dir, key+".har")
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// sanitize turns an arbitrary session id into a safe single-segment filename.
func sanitize(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown"
	}
	s = unsafeChars.ReplaceAllString(s, "_")
	if s == "" || s == "." || s == ".." {
		return "unknown"
	}
	if len(s) > 128 {
		s = s[:128]
	}
	return s
}
