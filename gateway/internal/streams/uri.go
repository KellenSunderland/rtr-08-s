package streams

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// URIPolicy is the stream URI allow-list.
type URIPolicy struct {
	MediaRoot      string
	AllowedSchemes []string // file, device, rtsp, rtmp, rtmps, srt, http, https (and fake for tests/dev)
	AllowedHosts   []string // for rtsp/rtmp/rtmps/srt/http/https; empty = no remote hosts allowed
}

// URIError is a validation failure (maps to HTTP 400).
type URIError struct{ Msg string }

func (e *URIError) Error() string { return e.Msg }

func bad(f string, a ...any) error { return &URIError{fmt.Sprintf(f, a...)} }

var (
	deviceRe = regexp.MustCompile(`^device:(\d{1,3})$`)
	schemeRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*):`)
)

func (p URIPolicy) schemeAllowed(s string) bool {
	for _, a := range p.AllowedSchemes {
		if strings.EqualFold(a, s) {
			return true
		}
	}
	return false
}

func (p URIPolicy) hostAllowed(h string) bool {
	for _, a := range p.AllowedHosts {
		if strings.EqualFold(a, h) {
			return true
		}
	}
	return false
}

// Validate checks uri against the policy and returns the URI/path to hand to
// the capture layer (file paths are returned as resolved absolute paths).
func (p URIPolicy) Validate(uri string) (string, error) {
	if uri == "" || len(uri) > 2048 {
		return "", bad("uri must be 1..2048 characters")
	}
	if strings.ContainsAny(uri, "\x00\r\n") {
		return "", bad("uri contains control characters")
	}
	scheme := "file"
	if m := schemeRe.FindStringSubmatch(uri); m != nil {
		scheme = strings.ToLower(m[1])
	}
	if !p.schemeAllowed(scheme) {
		return "", bad("uri scheme %q is not allowed", scheme)
	}
	switch scheme {
	case "file":
		return p.validateFile(uri)
	case "device":
		if !deviceRe.MatchString(uri) {
			return "", bad("device uri must look like device:N")
		}
		return uri, nil
	case "rtsp", "http", "https":
		u, err := url.Parse(uri)
		if err != nil || u.Hostname() == "" {
			return "", bad("invalid %s uri", scheme)
		}
		if u.User != nil {
			return "", bad("credentials in uri are not allowed")
		}
		if !p.hostAllowed(u.Hostname()) {
			return "", bad("host %q is not allowed", u.Hostname())
		}
		return uri, nil
	case "rtmp", "rtmps", "srt":
		return p.validateIngest(uri, scheme)
	case "fake":
		return uri, nil
	}
	return "", bad("unsupported scheme %q", scheme)
}

// validateIngest validates the push-style ingest schemes (rtmp, rtmps, srt).
// Operators opt in per scheme through AllowedSchemes.
func (p URIPolicy) validateIngest(uri, scheme string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Hostname() == "" {
		return "", bad("invalid %s uri", scheme)
	}
	if u.User != nil {
		return "", bad("credentials in uri are not allowed")
	}
	// Ingest endpoints are addressed by host:port; an explicit port means the
	// operator is pointing at a specific media server.
	if u.Port() == "" && !p.hostAllowed(u.Hostname()) {
		return "", bad("host %q is not allowed", u.Hostname())
	}
	return uri, nil
}

func (p URIPolicy) validateFile(uri string) (string, error) {
	path := uri
	switch {
	case strings.HasPrefix(strings.ToLower(path), "file://"):
		path = path[len("file://"):]
	case strings.HasPrefix(strings.ToLower(path), "file:"):
		path = path[len("file:"):]
	}
	if path == "" {
		return "", bad("empty file path")
	}
	if strings.Contains(path, "%") {
		if u, err := url.PathUnescape(path); err == nil {
			path = u
		}
	}
	for _, seg := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return "", bad("path traversal is not allowed")
		}
	}
	if p.MediaRoot == "" {
		return "", bad("file streams are disabled (no media root configured)")
	}
	root, err := filepath.EvalSymlinks(p.MediaRoot)
	if err != nil {
		return "", bad("media root unavailable")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", bad("media root unavailable")
	}
	full := path
	if !filepath.IsAbs(full) {
		full = filepath.Join(root, full)
	}
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", bad("file not found under media root")
		}
		return "", bad("cannot resolve file path")
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", bad("file is outside the media root")
	}
	st, err := os.Stat(resolved)
	if err != nil || !st.Mode().IsRegular() {
		return "", bad("not a regular file")
	}
	return resolved, nil
}
