package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Filesystem inspection routes for the console: does a path exist, what does a
// directory hold, what does a file read as. They serve the machine the daemon
// runs on — the box the hermes sessions and their tool calls actually touched —
// which is the only place that can answer, and the only one whose answer is
// true for what the session worked on.
//
// The surface is deliberately narrow. Every route sits under
// /sessions/{id}/fs/... and names a real session id, so the same lookup every
// other route uses gates them all; the browser finds candidate paths itself
// (rendering is its job) and asks in batched stats. Nothing here writes: no
// create, no delete, no rename, no execute. The file route caps what it will
// read (default 512 KB, ceiling 2 MB), reports binary files as binary with a
// hex head rather than painting mojibake into JSON, and answers a real error
// string when a path is unreadable.

// fsStat is one path's existence answer. Err is set when the path answers to
// nothing on this box; Resolved names what "~" became.
type fsStat struct {
	Given    string `json:"given"`
	Resolved string `json:"resolved,omitempty"`
	Type     string `json:"type,omitempty"` // dir | file | other
	Size     int64  `json:"size,omitempty"`
	Modified string `json:"modified,omitempty"`
	Err      string `json:"err,omitempty"`
}

// fsEntry is one row of a directory listing.
type fsEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size,omitempty"`
}

func fsType(fi os.FileInfo) string {
	switch {
	case fi.IsDir():
		return "dir"
	case fi.Mode().IsRegular():
		return "file"
	}
	return "other"
}

// fsResolvePath expands a leading ~ to the user's home and cleans the result.
// A bare "/" or "~" is refused: listing the filesystem root serves no session
// and reads terribly.
func fsResolvePath(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "/" || raw == "~" {
		return "", false
	}
	if strings.HasPrefix(raw, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		raw = filepath.Join(home, strings.TrimPrefix(raw, "~"))
	}
	return filepath.Clean(raw), true
}

// handleFSStat serves POST /sessions/{id}/fs/stat {paths:[...]}: one batched
// existence answer for up to 200 candidates. A batch is the whole traffic
// pattern the feature has — the console caches answers, so painted text costs
// one request no matter how many paths it carries.
func (r *Registry) handleFSStat(w http.ResponseWriter, req *http.Request) {
	if _, _, err := r.fsSession(req); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	var body struct {
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil || len(body.Paths) == 0 {
		writeErr(w, http.StatusBadRequest, "paths is required")
		return
	}
	if len(body.Paths) > 200 {
		body.Paths = body.Paths[:200]
	}
	out := make([]fsStat, 0, len(body.Paths))
	for _, raw := range body.Paths {
		p, ok := fsResolvePath(raw)
		if !ok {
			out = append(out, fsStat{Given: raw, Err: "refused"})
			continue
		}
		fi, err := os.Stat(p)
		if err != nil {
			out = append(out, fsStat{Given: raw, Err: "no"})
			continue
		}
		st := fsStat{Given: raw, Resolved: p, Type: fsType(fi), Size: fi.Size()}
		if fi.Mode().IsRegular() {
			st.Modified = fi.ModTime().UTC().Format(time.RFC3339)
		}
		out = append(out, st)
	}
	writeJSON(w, http.StatusOK, map[string]any{"stats": out})
}

// handleFSList serves GET /sessions/{id}/fs/list?path=D: one directory's
// entries, directories first then alphabetical. Dot files stay hidden except
// the two a developer opens on purpose.
func (r *Registry) handleFSList(w http.ResponseWriter, req *http.Request) {
	if _, _, err := r.fsSession(req); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	p, ok := fsResolvePath(req.URL.Query().Get("path"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "path is required")
		return
	}
	fi, err := os.Stat(p)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if !fi.IsDir() {
		writeErr(w, http.StatusBadRequest, "not a directory")
		return
	}
	dirents, err := os.ReadDir(p)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]fsEntry, 0, len(dirents))
	for _, de := range dirents {
		name := de.Name()
		if strings.HasPrefix(name, ".") && name != ".gitignore" && name != ".env" {
			continue
		}
		fi2, err2 := de.Info()
		if err2 != nil {
			continue // raced a delete; the entry is gone
		}
		e := fsEntry{Name: name, Type: fsType(fi2)}
		if !fi2.IsDir() {
			e.Size = fi2.Size()
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Type == "dir") != (out[j].Type == "dir") {
			return out[i].Type == "dir"
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, http.StatusOK, map[string]any{"path": p, "entries": out, "count": len(out)})
}

// handleFSFile serves GET /sessions/{id}/fs/file?path=F: a regular file as
// text, capped, binary named as binary. tail=1 serves the file's LAST window
// (the same max_bytes cap) instead of refusing an oversized file — a log being
// written while it is watched grows past any cap from the front, and tail -f
// reads the end. The response carries tail:true and offset (the byte the text
// starts at) when tailing, so a caller can tell a window from a whole file.
func (r *Registry) handleFSFile(w http.ResponseWriter, req *http.Request) {
	if _, _, err := r.fsSession(req); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	p, ok := fsResolvePath(req.URL.Query().Get("path"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "path is required")
		return
	}
	fi, err := os.Stat(p)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if !fi.Mode().IsRegular() {
		writeErr(w, http.StatusBadRequest, "not a regular file")
		return
	}
	tail := req.URL.Query().Get("tail") == "1"
	maxBytes := int64(512 * 1024)
	if req.URL.Query().Has("max_bytes") {
		if n, err2 := strconv.Atoi(req.URL.Query().Get("max_bytes")); err2 == nil && n > 0 && int64(n) < 2<<20 {
			maxBytes = int64(n)
		}
	}
	var data []byte
	var offset int64
	if tail && fi.Size() > maxBytes {
		// The tail window: open, seek to size-cap, read to EOF. The file may
		// grow between stat and read — the window then starts a little early,
		// which is exactly what a follower wants; it never reads past EOF.
		f, err2 := os.Open(p)
		if err2 != nil {
			writeErr(w, http.StatusInternalServerError, err2.Error())
			return
		}
		start := fi.Size() - maxBytes
		// Start at a line boundary when the window's first byte splits one:
		// seek back to the newline so the window opens with a whole line.
		head := make([]byte, 1)
		if _, err3 := f.ReadAt(head, start); err3 == nil && head[0] != '\n' {
			var back []byte
			if start > 512 {
				back = make([]byte, 512)
				f.ReadAt(back, start-512)
				if i := bytes.LastIndexByte(back, '\n'); i >= 0 {
					start = start - 512 + int64(i) + 1
				}
			} else {
				start = 0
			}
		}
		data = make([]byte, fi.Size()-start)
		_, err2 = f.ReadAt(data, start)
		f.Close()
		if err2 != nil && err2 != io.EOF {
			writeErr(w, http.StatusInternalServerError, err2.Error())
			return
		}
		offset = start
	} else {
		data, err = os.ReadFile(p)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !tail && fi.Size() > maxBytes {
			writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
				"file is %d bytes; the viewer reads up to %d (or ask with tail=1)", fi.Size(), maxBytes))
			return
		}
	}
	if fsIsBinary(data) {
		n := len(data)
		if n > 16 {
			n = 16
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"path": p, "binary": true, "size": fi.Size(),
			"head": fmt.Sprintf("% x", data[:n]),
		})
		return
	}
	out := map[string]any{
		"path":   p,
		"text":   string(data),
		"size":   fi.Size(),
		"binary": false,
		"lines":  1 + strings.Count(string(data), "\n"),
		"mod":    fi.ModTime().UTC().Format(time.RFC3339),
	}
	if tail {
		out["tail"] = true
		out["offset"] = offset
	}
	writeJSON(w, http.StatusOK, out)
}

// fsIsBinary: a NUL anywhere in the head — the same sniffing rule git and grep
// share. UTF-16 reads as binary too; labelling it beats painting mojibake.
func fsIsBinary(data []byte) bool {
	n := len(data)
	if n > 8000 {
		n = 8000
	}
	return bytes.IndexByte(data[:n], 0) >= 0
}

// fsImageKind names an image by its CONTENT, never by its extension: the
// console renders these inline, so a file called .jpg that holds a PNG must be
// painted as the PNG it is, and a .txt that holds neither must not be painted
// at all. Returns "" for anything else, which is the "not an image" answer.
func fsImageKind(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(data, []byte("\xff\xd8\xff")):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif"
	case len(data) > 12 && bytes.HasPrefix(data, []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp"
	}
	return ""
}

// fsRawCap is the ceiling for /fs/raw. Screenshots and photos are the target
// and they run to a few MB; this is a viewer, not a downloader.
const fsRawCap = 16 << 20

// handleFSRaw serves the file's BYTES with its own content type, for the things
// a JSON text answer cannot carry: an image the console renders in the viewer.
// /fs/file is the text route and deliberately refuses binary content (a hex head
// instead of mojibake inside JSON); this one is the opposite trade — raw bytes,
// no JSON envelope, and only for a caller that asked for exactly that.
//
// Only images are served: the route exists so a jpg/png/gif in a file list can be
// looked at, and widening it to "any bytes" would turn the console into a file
// exfiltrator for a session's box. Unknown content is a 415 with the reason.
func (r *Registry) handleFSRaw(w http.ResponseWriter, req *http.Request) {
	if _, _, err := r.fsSession(req); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	p, ok := fsResolvePath(req.URL.Query().Get("path"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "path is required")
		return
	}
	fi, err := os.Stat(p)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if !fi.Mode().IsRegular() {
		writeErr(w, http.StatusBadRequest, "not a regular file")
		return
	}
	if fi.Size() > fsRawCap {
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"file is %d bytes; raw serving stops at %d", fi.Size(), fsRawCap))
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	kind := fsImageKind(data)
	if kind == "" {
		writeErr(w, http.StatusUnsupportedMediaType, "not an image (png, jpeg, gif, webp)")
		return
	}
	// A viewer asks again on every open; the file is local, and a stale image is
	// indistinguishable from a broken renderer, so no caching.
	w.Header().Set("Content-Type", kind)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// fsSession is the route gate: the path names a session id the daemon knows.
// The daemon has no per-session file grant to enforce — everything it serves
// comes from the same box — so the lookup keeps the route in the session
// family while the console's own bearer token and content password do the
// authenticating, exactly as for every other route.
func (r *Registry) fsSession(req *http.Request) (PaneID, *PaneStatus, error) {
	return r.findPaneBySession(req.PathValue("id"))
}
