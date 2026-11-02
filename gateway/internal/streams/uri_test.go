package streams

import (
	"os"
	"path/filepath"
	"testing"
)

func TestURIValidation(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "media")
	outside := filepath.Join(base, "secret")
	for _, d := range []string{root, outside, filepath.Join(root, "sub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p string) { _ = os.WriteFile(p, []byte("x"), 0o644) }
	write(filepath.Join(root, "a.mp4"))
	write(filepath.Join(root, "sub", "b.mp4"))
	write(filepath.Join(outside, "s.mp4"))
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "s.mp4"), filepath.Join(root, "link.mp4")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "a.mp4"), filepath.Join(root, "inside.mp4")); err != nil {
		t.Fatal(err)
	}
	p := URIPolicy{MediaRoot: root, AllowedSchemes: []string{"file", "device", "rtsp", "http", "rtmp", "srt"}, AllowedHosts: []string{"cam.local", "Other.Example"}}

	ok := []string{"a.mp4", "sub/b.mp4", "file:a.mp4", "file://sub/b.mp4", "inside.mp4", filepath.Join(root, "a.mp4"), "device:0", "device:12",
		"rtsp://cam.local:554/stream", "http://other.example/mjpeg",
		"rtmp://cam.local/live/main", "srt://cam.local:9000?streamid=a"}
	for _, u := range ok {
		if _, err := p.Validate(u); err != nil {
			t.Errorf("%q should be valid: %v", u, err)
		}
	}
	bad := []string{"", "../secret/s.mp4", "sub/../../secret/s.mp4", "a/../../x", "%2e%2e/secret/s.mp4", "..\\secret\\s.mp4",
		"escape/s.mp4", "link.mp4", filepath.Join(outside, "s.mp4"), "/etc/passwd", "file:///etc/passwd", "missing.mp4", "sub",
		"device:abc", "device:1234", "device:-1",
		"https://cam.local/x", // https scheme not allowed
		"rtsp://evil.example/x", "rtsp://user:pw@cam.local/x", "ftp://cam.local/x", "javascript:alert(1)", "a.mp4\x00.png",
		"rtsp://cam.local@evil.example/x",
		"rtmp://evil.example/live", "rtmp://user:pw@cam.local/live", "rtmps://cam.local/live", // rtmps not enabled
		"srt://evil.example/x?streamid=a"}
	for _, u := range bad {
		if _, err := p.Validate(u); err == nil {
			t.Errorf("%q should be rejected", u)
		}
	}
	// resolved path is returned for files
	got, _ := p.Validate("inside.mp4")
	want, _ := filepath.EvalSymlinks(filepath.Join(root, "a.mp4"))
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
	// no media root => files disabled
	if _, err := (URIPolicy{AllowedSchemes: []string{"file"}}).Validate("a.mp4"); err == nil {
		t.Error("file without media root")
	}
	// remote hosts denied when list empty
	if _, err := (URIPolicy{AllowedSchemes: []string{"rtsp"}}).Validate("rtsp://cam.local/x"); err == nil {
		t.Error("empty host list must deny")
	}
}
