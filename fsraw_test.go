package main

// The raw route exists for one reason: a jpg/png/gif named in a session's files
// is something a human wants to LOOK at, and /fs/file cannot carry it — that
// route answers text, and binary content there is a hex head inside JSON. These
// tests pin the content sniffing (never the file extension) and the refusal,
// because a route that serves "any bytes" would be a file exfiltrator.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The session id every test below names; any id works as long as the registry's
// pane carries the same one.
const fsRawTestSID = "20260101_000000_abcdef"

// fsRawTestReq is a request through the route's own pattern: SetPathValue is
// what the mux does, and a bare NewRequest does not, so the handler would see an
// empty session id and 404 on its gate.
func fsRawTestReq(p string) *http.Request {
	req := httptest.NewRequest("GET", "/sessions/"+fsRawTestSID+"/fs/raw?path="+p, nil)
	req.SetPathValue("id", fsRawTestSID)
	return req
}

func fsRawTestRegistry(t *testing.T, sid string) *Registry {
	t.Helper()
	// fsSession resolves a session id to a pane; giving the registry one pane
	// that carries the id is enough to open the route's gate.
	r := &Registry{panes: map[PaneID]PaneStatus{
		"%1": {Pane: Pane{ID: "%1"}, HasHermes: true, SessionID: sid},
	}}
	return r
}

func TestFsImageKindSniffsContentNotExtension(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	jpeg := append([]byte("\xff\xd8\xff\xe0"), make([]byte, 16)...)
	gif := append([]byte("GIF89a"), make([]byte, 16)...)
	webp := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 4)...)
	text := []byte("this is not an image at all, just prose\n")

	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"png", png, "image/png"},
		{"jpeg", jpeg, "image/jpeg"},
		{"gif", gif, "image/gif"},
		{"webp", webp, "image/webp"},
		{"text", text, ""},
		{"empty", nil, ""},
		// A .png extension on text must NOT be painted as an image.
		{"misnamed", text, ""},
	}
	for _, c := range cases {
		if got := fsImageKind(c.data); got != c.want {
			t.Errorf("fsImageKind(%s) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestFsRawServesImageBytesWithItsOwnType(t *testing.T) {
	dir := t.TempDir()
	r := fsRawTestRegistry(t, fsRawTestSID)
	p := filepath.Join(dir, "shot.jpg")
	// PNG bytes under a .jpg name: the CONTENT decides the type, which is the
	// whole point of sniffing rather than trusting the extension.
	data := append([]byte("\x89PNG\r\n\x1a\n"), []byte("payload-payload")...)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	rec := httptest.NewRecorder()
	r.handleFSRaw(rec, fsRawTestReq(p))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content type = %q, want image/png (the bytes, not the name)", ct)
	}
	if got := rec.Body.Bytes(); string(got) != string(data) {
		t.Fatalf("body is not the file's bytes: %d vs %d", len(got), len(data))
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control = %q, want no-store", cc)
	}
}

func TestFsRawRefusesNonImage(t *testing.T) {
	dir := t.TempDir()
	r := fsRawTestRegistry(t, fsRawTestSID)
	p := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(p, []byte("plain prose, no magic number\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	rec := httptest.NewRecorder()
	r.handleFSRaw(rec, fsRawTestReq(p))

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415 (only images are served raw)", rec.Code)
	}
}

func TestFsRawRefusesOversizedFile(t *testing.T) {
	dir := t.TempDir()
	r := fsRawTestRegistry(t, fsRawTestSID)
	p := filepath.Join(dir, "huge.png")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// A real PNG header, then a sparse body past the cap.
	if _, err := f.Write([]byte("\x89PNG\r\n\x1a\n")); err != nil {
		t.Fatalf("header: %v", err)
	}
	if err := f.Truncate(fsRawCap + 1); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	f.Close()

	rec := httptest.NewRecorder()
	r.handleFSRaw(rec, fsRawTestReq(p))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}
