package web

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// pathStatic is the prefix the embedded assets are served under. The trailing
// slash makes it a subtree pattern.
const pathStatic = "/static/"

// staticCacheControl is as long as HTTP allows, because every asset URL carries
// a hash of the assets themselves. A build that changes a stylesheet changes
// its URL, so nothing has to expire for a deployment to be seen.
const staticCacheControl = "public, max-age=31536000, immutable"

// staticHandler serves the embedded CSS and JavaScript.
func (s *Server) staticHandler() http.Handler {
	root, err := fs.Sub(staticFS, "static")
	if err != nil {
		// The directory is embedded at compile time; if it is missing, the
		// binary is broken and every page would be too.
		panic(fmt.Sprintf("embedded static assets: %v", err))
	}

	files := http.FileServerFS(root)

	return http.StripPrefix(pathStatic, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Directory listings would expose the layout of the asset tree and are
		// never something a page asks for.
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", staticCacheControl)
		files.ServeHTTP(w, r)
	}))
}

// assetVersion hashes every embedded static file into a short stamp, used as
// the cache-busting query on asset URLs.
//
// The build revision would be the obvious choice, but a working tree rebuilt
// during development keeps the same revision while its stylesheet changes,
// which is exactly when a stale cache costs the most time. Hashing the content
// answers the question actually being asked: are these the same bytes?
func assetVersion() (string, error) {
	root, err := fs.Sub(staticFS, "static")
	if err != nil {
		return "", fmt.Errorf("embedded static assets: %w", err)
	}

	sum := sha256.New()
	err = fs.WalkDir(root, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		// The name goes in as well as the contents, so that renaming a file
		// changes the stamp even when no bytes change. hash.Hash.Write is
		// documented never to return an error.
		_, _ = fmt.Fprintf(sum, "%s\x00", path.Clean(name))

		f, err := root.Open(name)
		if err != nil {
			return err
		}
		defer f.Close() //nolint:errcheck // read-only file from an embedded FS

		_, err = io.Copy(sum, f)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("hash static assets: %w", err)
	}

	return hex.EncodeToString(sum.Sum(nil))[:12], nil
}
