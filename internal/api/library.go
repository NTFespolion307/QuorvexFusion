package api

import (
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/NTFespolion307/QuorvexFusion/internal/blobstore"
	"github.com/NTFespolion307/QuorvexFusion/internal/store"
)

// Resumable uploads (used by the browser) and the file library.
//
//	POST   /api/v1/uploads                    start an upload -> {"id"}
//	HEAD   /api/v1/uploads/{id}               X-Upload-Offset: bytes received so far
//	PUT    /api/v1/uploads/{id}?offset=N      next chunk (body), from offset N -> {"offset"}
//	POST   /api/v1/uploads/{id}/finish        {"size", "path"} -> {"sha256", "size"[, "file"]}
//	                                          (with a path, the file is added to the library)
//
//	GET    /api/v1/files[?path=folder]        list library files
//	POST   /api/v1/files                      {"path", "sha256"}: add already uploaded content
//	DELETE /api/v1/files?path=P               remove a file or folder
//	GET    /api/v1/files/content?path=P       download a library file (Range supported)

func (s *Server) uploadStart(w http.ResponseWriter, r *http.Request) {
	id, err := s.c.Blobs().NewSession()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (s *Server) uploadStatus(w http.ResponseWriter, r *http.Request) {
	n, err := s.c.Blobs().SessionSize(r.PathValue("id"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("X-Upload-Offset", strconv.FormatInt(n, 10))
	w.WriteHeader(http.StatusOK)
}

func (s *Server) uploadChunk(w http.ResponseWriter, r *http.Request) {
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		writeError(w, http.StatusBadRequest, "offset query parameter required")
		return
	}
	n, err := s.c.Blobs().SessionWrite(r.PathValue("id"), offset, r.Body)
	var oe *blobstore.OffsetError
	switch {
	case errors.As(err, &oe):
		writeJSON(w, http.StatusConflict, map[string]any{"error": oe.Error(), "offset": oe.Have})
	case errors.Is(err, blobstore.ErrNoSession):
		writeError(w, http.StatusNotFound, err.Error())
	case err != nil:
		// The connection probably broke mid-chunk; the client asks for the
		// offset and continues.
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]int64{"offset": n})
	}
}

func (s *Server) uploadFinish(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Size int64  `json:"size"`
		Path string `json:"path"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sha, size, err := s.c.Blobs().SessionFinish(r.PathValue("id"), req.Size)
	var oe *blobstore.OffsetError
	switch {
	case errors.As(err, &oe):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "upload incomplete", "offset": oe.Have})
		return
	case errors.Is(err, blobstore.ErrNoSession):
		writeError(w, http.StatusNotFound, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := map[string]any{"sha256": sha, "size": size}
	if req.Path != "" {
		f, err := s.c.AddLibraryFile(req.Path, sha)
		if err != nil {
			writeErr(w, err)
			return
		}
		resp["file"] = f
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	list, err := s.c.Store().LibraryFiles(strings.Trim(r.URL.Query().Get("path"), "/"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if list == nil {
		list = []*store.LibraryFile{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) addFile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	f, err := s.c.AddLibraryFile(req.Path, req.SHA256)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

func (s *Server) deleteFiles(w http.ResponseWriter, r *http.Request) {
	n, err := s.c.DeleteLibraryPath(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"deleted": n})
}

func (s *Server) fileContent(w http.ResponseWriter, r *http.Request) {
	f, err := s.c.Store().LibraryFile(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, err)
		return
	}
	data, err := s.c.Blobs().Open(f.SHA256)
	if err != nil {
		writeError(w, http.StatusGone, "file content is no longer stored")
		return
	}
	defer data.Close()
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path.Base(f.Path)))
	w.Header().Set("X-Content-SHA256", f.SHA256)
	http.ServeContent(w, r, path.Base(f.Path), time.Time{}, data)
}
