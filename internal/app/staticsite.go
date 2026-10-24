package app

import (
	"errors"
	"io/fs"
	"log"
	"net/http"
)

// NewStaticHandler serves the built frontend from dir.
//
// A request for a path that names no file under dir (including a directory)
// is served dir/index.html instead, so a client-side route such as /admin
// survives a hard refresh. A path that does name a file but cannot be opened
// is a real 500, not masked by that fallback. If index.html itself is
// missing, as before the frontend has ever been built, the answer is a plain
// 404.
//
// Paths are resolved through http.Dir, which confines them to dir.
func NewStaticHandler(dir string) http.Handler {
	root := http.Dir(dir)
	files := http.FileServer(root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isFile, err := isRegularFile(root, r.URL.Path)
		switch {
		case err != nil:
			staticError(w, r, err)
		case isFile:
			files.ServeHTTP(w, r)
		default:
			serveIndex(w, r, root)
		}
	})
}

// isRegularFile reports whether name exists under root and is not a
// directory. A missing name is not an error.
func isRegularFile(root http.FileSystem, name string) (bool, error) {
	f, err := root.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	return !info.IsDir(), nil
}

// serveIndex serves root's index.html whatever the request path. It is
// marked no-cache so a browser always revalidates it, and so always picks up
// the content-hashed asset names of the latest build.
func serveIndex(w http.ResponseWriter, r *http.Request, root http.FileSystem) {
	f, err := root.Open("/index.html")
	if errors.Is(err, fs.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		staticError(w, r, err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		staticError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", info.ModTime(), f)
}

// staticError answers a request whose file could not be read. The cause,
// which names a path on disk, is logged rather than returned.
func staticError(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("static: %s %s: %v", r.Method, r.URL.Path, err)
	http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
}
