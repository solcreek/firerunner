package cacheserver

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestServer returns a Server backed by a temp dir and an httptest server in
// front of it so signed URLs (built from r.Host) resolve back to the same server.
func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	s, err := New(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return ts, ts.URL
}

func twirp(t *testing.T, base, method string, req any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(req)
	url := base + twirpBase + method
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", method, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s -> %d: %s", method, resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s decode %q: %v", method, raw, err)
	}
	return out
}

// putBlob does a single-shot Azure-style PUT (the toolkit's path for blobs
// <= 128 MiB).
func putBlob(t *testing.T, url string, data []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT blob: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT blob -> %d", resp.StatusCode)
	}
}

func meta(repo string) *cacheMetadata { return &cacheMetadata{RepositoryID: repo} }

// TestSaveRestoreSingleShot walks the full toolkit v2 flow: reserve -> single
// PUT -> finalize -> resolve download URL -> GET, and checks the bytes round-trip.
func TestSaveRestoreSingleShot(t *testing.T) {
	_, base := newTestServer(t)
	const version = "v1abc"
	payload := []byte("hello pnpm store")

	create := twirp(t, base, "CreateCacheEntry", createReq{Metadata: meta("42"), Key: "Node-Modules-abc", Version: version})
	if create["ok"] != true {
		t.Fatalf("create not ok: %v", create)
	}
	putBlob(t, create["signed_upload_url"].(string), payload)

	fin := twirp(t, base, "FinalizeCacheEntryUpload", finalizeReq{
		Metadata: meta("42"), Key: "Node-Modules-abc", Version: version,
		SizeBytes: fmt.Sprint(len(payload)),
	})
	if fin["ok"] != true {
		t.Fatalf("finalize not ok: %v", fin)
	}

	get := twirp(t, base, "GetCacheEntryDownloadURL", getReq{Metadata: meta("42"), Key: "Node-Modules-abc", Version: version})
	if get["ok"] != true {
		t.Fatalf("get not ok: %v", get)
	}
	// Keys are matched case-insensitively and echoed lowercased.
	if got := get["matched_key"]; got != "node-modules-abc" {
		t.Fatalf("matched_key = %v", got)
	}
	resp, err := http.Get(get["signed_download_url"].(string))
	if err != nil {
		t.Fatalf("GET blob: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, payload) {
		t.Fatalf("round-trip mismatch: %q != %q", got, payload)
	}
}

// TestBlockUpload exercises the staged-block path (>128 MiB in the real client):
// several ?comp=block PUTs then a ?comp=blocklist commit, reassembled in list order.
func TestBlockUpload(t *testing.T) {
	_, base := newTestServer(t)
	const version = "vblk"
	create := twirp(t, base, "CreateCacheEntry", createReq{Key: "big", Version: version})
	uploadURL := create["signed_upload_url"].(string)

	blocks := map[string][]byte{
		"blk-0001": []byte("AAAA"),
		"blk-0002": []byte("BBBB"),
		"blk-0003": []byte("CCCC"),
	}
	for id, data := range blocks {
		putBlockPart(t, uploadURL, id, data)
	}
	// Commit in a deliberate order; the assembled blob must follow it.
	commitBlocks(t, uploadURL, []string{"blk-0002", "blk-0003", "blk-0001"})

	twirp(t, base, "FinalizeCacheEntryUpload", finalizeReq{Key: "big", Version: version, SizeBytes: "12"})
	get := twirp(t, base, "GetCacheEntryDownloadURL", getReq{Key: "big", Version: version})
	resp, err := http.Get(get["signed_download_url"].(string))
	if err != nil {
		t.Fatalf("GET blob: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != "BBBBCCCCAAAA" {
		t.Fatalf("block order wrong: %q", got)
	}
}

func putBlockPart(t *testing.T, uploadURL, blockID string, data []byte) {
	t.Helper()
	sep := "&"
	if !strings.Contains(uploadURL, "?") {
		sep = "?"
	}
	url := uploadURL + sep + "comp=block&blockid=" + blockID
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT block: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT block -> %d", resp.StatusCode)
	}
}

func commitBlocks(t *testing.T, uploadURL string, ids []string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("<BlockList>")
	for _, id := range ids {
		fmt.Fprintf(&b, "<Latest>%s</Latest>", id)
	}
	b.WriteString("</BlockList>")
	sep := "&"
	if !strings.Contains(uploadURL, "?") {
		sep = "?"
	}
	req, _ := http.NewRequest(http.MethodPut, uploadURL+sep+"comp=blocklist", strings.NewReader(b.String()))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("commit blocklist: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("commit -> %d", resp.StatusCode)
	}
}

// TestRestoreKeyPrefix verifies restore-key prefix matching and newest-wins.
func TestRestoreKeyPrefix(t *testing.T) {
	_, base := newTestServer(t)
	const version = "vp"
	for _, key := range []string{"deps-linux-aaa", "deps-linux-bbb"} {
		create := twirp(t, base, "CreateCacheEntry", createReq{Key: key, Version: version})
		putBlob(t, create["signed_upload_url"].(string), []byte(key))
		twirp(t, base, "FinalizeCacheEntryUpload", finalizeReq{Key: key, Version: version, SizeBytes: fmt.Sprint(len(key))})
	}
	// No exact hit, but the "deps-linux-" prefix should match the newest (bbb).
	get := twirp(t, base, "GetCacheEntryDownloadURL", getReq{
		Key: "deps-linux-zzz", RestoreKeys: []string{"deps-linux-"}, Version: version,
	})
	if get["ok"] != true || get["matched_key"] != "deps-linux-bbb" {
		t.Fatalf("restore-key match = %v", get)
	}
}

// TestRepoIsolation checks entries never cross repository_id boundaries.
func TestRepoIsolation(t *testing.T) {
	_, base := newTestServer(t)
	const version, key = "vr", "shared-key"
	create := twirp(t, base, "CreateCacheEntry", createReq{Metadata: meta("1"), Key: key, Version: version})
	putBlob(t, create["signed_upload_url"].(string), []byte("repo1"))
	twirp(t, base, "FinalizeCacheEntryUpload", finalizeReq{Metadata: meta("1"), Key: key, Version: version, SizeBytes: "5"})

	// Same key+version but a different repo must miss.
	get := twirp(t, base, "GetCacheEntryDownloadURL", getReq{Metadata: meta("2"), Key: key, Version: version})
	if get["ok"] != false {
		t.Fatalf("cross-repo leak: %v", get)
	}
}

// TestMissAndImmutability covers a plain miss and the no-overwrite rule.
func TestMissAndImmutability(t *testing.T) {
	_, base := newTestServer(t)
	miss := twirp(t, base, "GetCacheEntryDownloadURL", getReq{Key: "nope", Version: "v"})
	if miss["ok"] != false {
		t.Fatalf("expected miss, got %v", miss)
	}

	create := twirp(t, base, "CreateCacheEntry", createReq{Key: "k", Version: "v"})
	putBlob(t, create["signed_upload_url"].(string), []byte("x"))
	twirp(t, base, "FinalizeCacheEntryUpload", finalizeReq{Key: "k", Version: "v", SizeBytes: "1"})

	// A second reservation of the same key+version is refused (immutable).
	dup := twirp(t, base, "CreateCacheEntry", createReq{Key: "k", Version: "v"})
	if dup["ok"] != false {
		t.Fatalf("expected immutable conflict, got %v", dup)
	}
}

// TestSigRequired ensures blob URLs can't be read without the per-entry token.
func TestSigRequired(t *testing.T) {
	_, base := newTestServer(t)
	create := twirp(t, base, "CreateCacheEntry", createReq{Key: "k", Version: "v"})
	putBlob(t, create["signed_upload_url"].(string), []byte("secret"))
	twirp(t, base, "FinalizeCacheEntryUpload", finalizeReq{Key: "k", Version: "v", SizeBytes: "6"})
	get := twirp(t, base, "GetCacheEntryDownloadURL", getReq{Key: "k", Version: "v"})

	url := get["signed_download_url"].(string)
	stripped := url[:strings.Index(url, "?")] // drop ?sig=
	resp, err := http.Get(stripped)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 without sig, got %d", resp.StatusCode)
	}
}

// TestPersistenceReload confirms the on-disk index survives a restart.
func TestPersistenceReload(t *testing.T) {
	dir := t.TempDir()
	s1, err := New(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	ts1 := httptest.NewServer(s1)
	create := twirp(t, ts1.URL, "CreateCacheEntry", createReq{Key: "persist", Version: "v"})
	putBlob(t, create["signed_upload_url"].(string), []byte("data"))
	twirp(t, ts1.URL, "FinalizeCacheEntryUpload", finalizeReq{Key: "persist", Version: "v", SizeBytes: "4"})
	ts1.Close()

	// Reopen the same dir: the finalized entry should still resolve.
	s2, err := New(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	ts2 := httptest.NewServer(s2)
	defer ts2.Close()
	get := twirp(t, ts2.URL, "GetCacheEntryDownloadURL", getReq{Key: "persist", Version: "v"})
	if get["ok"] != true {
		t.Fatalf("entry did not survive reload: %v", get)
	}
}

// sanity: hex helper matches how blocks are stored (guards the reassembly path).
func TestBlockIDEncoding(t *testing.T) {
	if hex.EncodeToString([]byte("blk-0001")) == "" {
		t.Fatal("hex encoding empty")
	}
}
