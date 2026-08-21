// Package cacheserver is a stdlib-only, disk-backed implementation of the
// GitHub Actions cache service (v2) protocol. It lets firerunner microVMs keep
// their `actions/cache` (and setup-*/pnpm/go-build caches built on it) on the
// host's local disk instead of GitHub's hosted cache, so dependency restore
// runs at LAN speed and without GitHub's per-repo quota.
//
// # Protocol
//
// The current `@actions/cache` toolkit (cache service v2, the only version on
// github.com since 2025-02) talks a small Twirp-over-HTTP JSON RPC to the URL
// in ACTIONS_RESULTS_URL, then streams the archive blob to/from a "signed" URL
// the RPC hands back. This server implements both halves:
//
//	POST /twirp/github.actions.results.api.v1.CacheService/CreateCacheEntry
//	POST /twirp/github.actions.results.api.v1.CacheService/FinalizeCacheEntryUpload
//	POST /twirp/github.actions.results.api.v1.CacheService/GetCacheEntryDownloadURL
//	PUT  /upload/<id>?sig=...     (Azure block-blob protocol: single-shot, or
//	                               ?comp=block&blockid=.. then ?comp=blocklist)
//	GET  /download/<id>?sig=...   (ranged GET + HEAD, i.e. Azure getProperties)
//
// The archive never passes through the RPC: CreateCacheEntry returns an upload
// URL back at this same server, the runner PUTs the blob there (Azure SDK, so
// small blobs are one PUT and large ones are staged blocks committed by a block
// list), and GetCacheEntryDownloadURL returns a download URL served straight
// off disk with Range support. Signed URLs are built from the request's Host
// header so each microVM reaches the server on its own per-slot gateway IP.
//
// # Isolation
//
// Entries are namespaced by the caller's repository_id (from the RPC metadata),
// so caches never leak across repositories. Within a repository the semantics
// match GitHub's cache (any job can read/write) minus ref-scoping: on a shared
// self-hosted server a job triggered by untrusted code can write an entry a
// later trusted job restores, so run one server per trust boundary. See the
// README security notes.
package cacheserver

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// twirpBase is the fixed Twirp service route the v2 toolkit POSTs to.
	twirpBase = "/twirp/github.actions.results.api.v1.CacheService/"

	// maxIncompleteAge bounds how long a reserved-but-unfinalized entry lingers
	// before the janitor reclaims it (a crashed or abandoned upload).
	maxIncompleteAge = 30 * time.Minute
)

// Entry is one cache record. Reserved on CreateCacheEntry, its blob is streamed
// to /upload/<ID>, and it becomes restorable once FinalizeCacheEntryUpload marks
// it Complete.
type Entry struct {
	ID        uint64 `json:"id"`
	Sig       string `json:"sig"`
	RepoID    string `json:"repo_id"`
	Key       string `json:"key"`
	Version   string `json:"version"`
	Size      int64  `json:"size"`
	Complete  bool   `json:"complete"`
	CreatedAt int64  `json:"created_at"`
	UsedAt    int64  `json:"used_at"`
}

// Server is a disk-backed GitHub Actions cache (v2) server. It is safe for
// concurrent use by many microVMs.
type Server struct {
	dir string
	log *slog.Logger
	mux *http.ServeMux

	mu      sync.Mutex
	entries map[uint64]*Entry
	nextID  uint64
}

// New opens (creating if needed) a cache store rooted at dir and returns a
// ready-to-serve Server. The on-disk index is loaded so caches survive restarts.
func New(dir string, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	for _, d := range []string{dir, filepath.Join(dir, "blobs"), filepath.Join(dir, "tmp")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("create cache dir %q: %w", d, err)
		}
	}
	s := &Server{
		dir:     dir,
		log:     log.With("module", "cacheserver"),
		entries: make(map[uint64]*Entry),
		nextID:  1,
	}
	if err := s.load(); err != nil {
		return nil, err
	}

	s.mux = http.NewServeMux()
	s.mux.HandleFunc("POST "+twirpBase+"CreateCacheEntry", s.handleCreate)
	s.mux.HandleFunc("POST "+twirpBase+"FinalizeCacheEntryUpload", s.handleFinalize)
	s.mux.HandleFunc("POST "+twirpBase+"GetCacheEntryDownloadURL", s.handleGetDownloadURL)
	s.mux.HandleFunc("PUT /upload/{id}", s.handleUpload)
	s.mux.HandleFunc("GET /download/{id}", s.handleDownload)
	s.mux.HandleFunc("HEAD /download/{id}", s.handleDownload)
	return s, nil
}

// ServeHTTP makes Server an http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// StartJanitor runs a background loop that reclaims abandoned (reserved but
// never finalized) entries until the returned stop function is called.
func (s *Server) StartJanitor() (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		s.gc()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				s.gc()
			}
		}
	}()
	return func() { close(done) }
}

// --- Twirp message shapes (proto3 JSON, useProtoFieldName: true; int64 as string) ---

type cacheMetadata struct {
	RepositoryID string `json:"repository_id"`
}

type createReq struct {
	Metadata *cacheMetadata `json:"metadata"`
	Key      string         `json:"key"`
	Version  string         `json:"version"`
}
type createResp struct {
	OK              bool   `json:"ok"`
	SignedUploadURL string `json:"signed_upload_url"`
	Message         string `json:"message,omitempty"`
}

type finalizeReq struct {
	Metadata  *cacheMetadata `json:"metadata"`
	Key       string         `json:"key"`
	SizeBytes string         `json:"size_bytes"`
	Version   string         `json:"version"`
}
type finalizeResp struct {
	OK      bool   `json:"ok"`
	EntryID string `json:"entry_id"`
	Message string `json:"message,omitempty"`
}

type getReq struct {
	Metadata    *cacheMetadata `json:"metadata"`
	Key         string         `json:"key"`
	RestoreKeys []string       `json:"restore_keys"`
	Version     string         `json:"version"`
}
type getResp struct {
	OK                bool   `json:"ok"`
	SignedDownloadURL string `json:"signed_download_url"`
	MatchedKey        string `json:"matched_key"`
	Message           string `json:"message,omitempty"`
}

func repoOf(m *cacheMetadata) string {
	if m == nil {
		return ""
	}
	return m.RepositoryID
}

// handleCreate reserves a new entry and returns an upload URL, or ok:false when
// a completed entry with the same key+version already exists (entries are
// immutable, matching GitHub's "another job may be creating this cache").
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		twirpError(w, http.StatusBadRequest, "malformed", err.Error())
		return
	}
	if req.Key == "" || req.Version == "" {
		twirpError(w, http.StatusBadRequest, "invalid_argument", "key and version are required")
		return
	}
	repo := repoOf(req.Metadata)
	key := strings.ToLower(req.Key)

	s.mu.Lock()
	if e := s.findExact(repo, key, req.Version); e != nil && e.Complete {
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, createResp{OK: false, Message: "cache entry already exists"})
		return
	}
	e := &Entry{
		ID:        s.nextID,
		Sig:       newToken(),
		RepoID:    repo,
		Key:       key,
		Version:   req.Version,
		Size:      -1,
		CreatedAt: time.Now().Unix(),
		UsedAt:    time.Now().Unix(),
	}
	s.nextID++
	s.entries[e.ID] = e
	if err := s.save(); err != nil {
		delete(s.entries, e.ID)
		s.mu.Unlock()
		twirpError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, createResp{
		OK:              true,
		SignedUploadURL: s.blobURL(r, "upload", e.ID, e.Sig),
	})
}

// handleFinalize marks a reserved entry complete once its blob is on disk.
func (s *Server) handleFinalize(w http.ResponseWriter, r *http.Request) {
	var req finalizeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		twirpError(w, http.StatusBadRequest, "malformed", err.Error())
		return
	}
	repo := repoOf(req.Metadata)
	key := strings.ToLower(req.Key)

	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.findReserved(repo, key, req.Version)
	if e == nil {
		writeJSON(w, http.StatusOK, finalizeResp{OK: false, Message: "no reserved cache entry to finalize"})
		return
	}
	fi, err := os.Stat(s.blobPath(e.ID))
	if err != nil {
		writeJSON(w, http.StatusOK, finalizeResp{OK: false, Message: "cache blob was not uploaded"})
		return
	}
	if want, err := strconv.ParseInt(req.SizeBytes, 10, 64); err == nil && want >= 0 && want != fi.Size() {
		writeJSON(w, http.StatusOK, finalizeResp{OK: false, Message: fmt.Sprintf("size mismatch: got %d, want %d", fi.Size(), want)})
		return
	}
	e.Size = fi.Size()
	e.Complete = true
	e.UsedAt = time.Now().Unix()
	if err := s.save(); err != nil {
		twirpError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.log.Info("cache entry finalized", "id", e.ID, "key", e.Key, "size", e.Size)
	writeJSON(w, http.StatusOK, finalizeResp{OK: true, EntryID: strconv.FormatUint(e.ID, 10)})
}

// handleGetDownloadURL resolves the best matching entry (exact key first, then
// restore-key prefixes in order, newest wins) and returns a download URL, or
// ok:false on a miss.
func (s *Server) handleGetDownloadURL(w http.ResponseWriter, r *http.Request) {
	var req getReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		twirpError(w, http.StatusBadRequest, "malformed", err.Error())
		return
	}
	repo := repoOf(req.Metadata)

	s.mu.Lock()
	e := s.match(repo, strings.ToLower(req.Key), req.RestoreKeys, req.Version)
	if e != nil {
		e.UsedAt = time.Now().Unix()
		_ = s.save()
	}
	s.mu.Unlock()

	if e == nil {
		writeJSON(w, http.StatusOK, getResp{OK: false})
		return
	}
	writeJSON(w, http.StatusOK, getResp{
		OK:                true,
		SignedDownloadURL: s.blobURL(r, "download", e.ID, e.Sig),
		MatchedKey:        e.Key,
	})
}

// handleUpload accepts the archive blob. It speaks the subset of the Azure
// block-blob API the cache toolkit's Azure SDK emits: a single-shot PUT for
// blobs up to 128 MiB, or staged blocks (?comp=block&blockid=..) committed by a
// block list (?comp=blocklist) for larger ones.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	e, ok := s.authEntry(w, r)
	if !ok {
		return
	}
	switch r.URL.Query().Get("comp") {
	case "block":
		s.uploadBlock(w, r, e)
	case "blocklist":
		s.commitBlockList(w, r, e)
	case "":
		s.uploadSingle(w, r, e)
	default:
		http.Error(w, "unsupported comp", http.StatusBadRequest)
	}
}

func (s *Server) uploadSingle(w http.ResponseWriter, r *http.Request, e *Entry) {
	if err := writeFile(s.blobPath(e.ID), r.Body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) uploadBlock(w http.ResponseWriter, r *http.Request, e *Entry) {
	blockID := r.URL.Query().Get("blockid")
	if blockID == "" {
		http.Error(w, "missing blockid", http.StatusBadRequest)
		return
	}
	dir := s.tmpDir(e.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := writeFile(filepath.Join(dir, hex.EncodeToString([]byte(blockID))), r.Body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) commitBlockList(w http.ResponseWriter, r *http.Request, e *Entry) {
	var list struct {
		Latest      []string `xml:"Latest"`
		Uncommitted []string `xml:"Uncommitted"`
		Committed   []string `xml:"Committed"`
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := xml.Unmarshal(body, &list); err != nil {
		http.Error(w, "bad block list: "+err.Error(), http.StatusBadRequest)
		return
	}
	ids := list.Latest
	ids = append(ids, list.Uncommitted...)
	ids = append(ids, list.Committed...)
	if len(ids) == 0 {
		http.Error(w, "empty block list", http.StatusBadRequest)
		return
	}

	dir := s.tmpDir(e.ID)
	out, err := os.Create(s.blobPath(e.ID))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer out.Close()
	for _, id := range ids {
		blockPath := filepath.Join(dir, hex.EncodeToString([]byte(id)))
		f, err := os.Open(blockPath)
		if err != nil {
			http.Error(w, "missing staged block: "+err.Error(), http.StatusBadRequest)
			return
		}
		_, err = io.Copy(out, f)
		f.Close()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	_ = os.RemoveAll(dir)
	w.WriteHeader(http.StatusCreated)
}

// handleDownload serves a completed entry's blob straight off disk. ServeContent
// gives Range + HEAD (the Azure getProperties + segmented download the toolkit
// uses) for free.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	e, ok := s.authEntry(w, r)
	if !ok {
		return
	}
	f, err := os.Open(s.blobPath(e.ID))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, strconv.FormatUint(e.ID, 10), fi.ModTime(), f)
}

// authEntry resolves and authorizes the {id} + ?sig= on a blob request.
func (s *Server) authEntry(w http.ResponseWriter, r *http.Request) (*Entry, bool) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return nil, false
	}
	s.mu.Lock()
	e := s.entries[id]
	s.mu.Unlock()
	if e == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return nil, false
	}
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("sig")), []byte(e.Sig)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return nil, false
	}
	return e, true
}

// --- matching (caller holds s.mu) ---

func (s *Server) findExact(repo, key, version string) *Entry {
	var best *Entry
	for _, e := range s.entries {
		if e.RepoID == repo && e.Key == key && e.Version == version {
			if newer(best, e) {
				best = e
			}
		}
	}
	return best
}

func (s *Server) findReserved(repo, key, version string) *Entry {
	var best *Entry
	for _, e := range s.entries {
		if !e.Complete && e.RepoID == repo && e.Key == key && e.Version == version {
			if newer(best, e) {
				best = e
			}
		}
	}
	return best
}

// match implements the toolkit's restore semantics: an exact key hit wins;
// otherwise each restore key is tried as a prefix in order, newest first.
func (s *Server) match(repo, key string, restoreKeys []string, version string) *Entry {
	if e := s.completeExact(repo, key, version); e != nil {
		return e
	}
	for _, rk := range restoreKeys {
		if e := s.completePrefix(repo, strings.ToLower(rk), version); e != nil {
			return e
		}
	}
	return nil
}

func (s *Server) completeExact(repo, key, version string) *Entry {
	var best *Entry
	for _, e := range s.entries {
		if e.Complete && e.RepoID == repo && e.Version == version && e.Key == key {
			if newer(best, e) {
				best = e
			}
		}
	}
	return best
}

func (s *Server) completePrefix(repo, prefix, version string) *Entry {
	var best *Entry
	for _, e := range s.entries {
		if e.Complete && e.RepoID == repo && e.Version == version && strings.HasPrefix(e.Key, prefix) {
			if newer(best, e) {
				best = e
			}
		}
	}
	return best
}

// newer reports whether e should replace best as the "newest" match: later
// CreatedAt wins, ties broken by higher ID (entries are created monotonically,
// so this is stable even when several land in the same second).
func newer(best, e *Entry) bool {
	if best == nil {
		return true
	}
	if e.CreatedAt != best.CreatedAt {
		return e.CreatedAt > best.CreatedAt
	}
	return e.ID > best.ID
}

// --- storage helpers ---

func (s *Server) blobPath(id uint64) string {
	return filepath.Join(s.dir, "blobs", strconv.FormatUint(id, 10))
}
func (s *Server) tmpDir(id uint64) string {
	return filepath.Join(s.dir, "tmp", strconv.FormatUint(id, 10))
}

func (s *Server) blobURL(r *http.Request, kind string, id uint64, sig string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/%s/%d?sig=%s", scheme, r.Host, kind, id, sig)
}

// gc reclaims reserved-but-unfinalized entries whose blob upload never completed.
func (s *Server) gc() {
	cutoff := time.Now().Add(-maxIncompleteAge).Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	var removed int
	for id, e := range s.entries {
		if !e.Complete && e.UsedAt < cutoff {
			_ = os.Remove(s.blobPath(id))
			_ = os.RemoveAll(s.tmpDir(id))
			delete(s.entries, id)
			removed++
		}
	}
	if removed > 0 {
		_ = s.save()
		s.log.Info("reclaimed abandoned cache entries", "count", removed)
	}
}

// --- index persistence (caller holds s.mu for save; load runs before serving) ---

func (s *Server) indexPath() string { return filepath.Join(s.dir, "index.json") }

func (s *Server) load() error {
	data, err := os.ReadFile(s.indexPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read cache index: %w", err)
	}
	var entries []*Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("decode cache index: %w", err)
	}
	for _, e := range entries {
		s.entries[e.ID] = e
		if e.ID >= s.nextID {
			s.nextID = e.ID + 1
		}
	}
	return nil
}

func (s *Server) save() error {
	entries := make([]*Entry, 0, len(s.entries))
	for _, e := range s.entries {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	tmp := s.indexPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.indexPath())
}

// --- small helpers ---

func writeFile(path string, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func newToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// twirpError writes a Twirp-shaped error envelope ({code,msg}) with an HTTP
// status. The toolkit only surfaces these for genuinely bad requests; cache
// misses and reservation conflicts are ok:false 200s handled inline.
func twirpError(w http.ResponseWriter, code int, twirpCode, msg string) {
	writeJSON(w, code, map[string]string{"code": twirpCode, "msg": msg})
}
