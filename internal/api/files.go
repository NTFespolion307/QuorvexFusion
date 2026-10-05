package api

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/NTFespolion307/QuorvexFusion/internal/blobstore"
	"github.com/NTFespolion307/QuorvexFusion/internal/store"
)

// File endpoints.
//
// Uploading (CLI, resumable):
//
//	HEAD /api/v1/blobs/{sha}                   200 + X-Blob-Size if stored,
//	                                           404 + X-Blob-Offset (bytes held so far)
//	PUT  /api/v1/blobs/{sha}?size=N&offset=M   body = bytes from offset M
//
// Uploading (browser, hash computed by the server):
//
//	POST /api/v1/blobs                         body = the file -> {"sha256", "size"}
//
// Downloading outputs:
//
//	GET /api/v1/tasks/{id}/outputs             list
//	GET /api/v1/tasks/{id}/files/{path}        one file (supports Range, so resumable)
//	GET /api/v1/jobs/{id}/outputs              list of all tasks' outputs
//	GET /api/v1/jobs/{id}/outputs.zip          everything as a zip

func (s *Server) blobStatus(w http.ResponseWriter, r *http.Request) {
	complete, have := s.c.Blobs().Status(r.PathValue("sha"))
	if complete {
		w.Header().Set("X-Blob-Size", strconv.FormatInt(have, 10))
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("X-Blob-Offset", strconv.FormatInt(have, 10))
	w.WriteHeader(http.StatusNotFound)
}

func (s *Server) blobPut(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	size, err1 := strconv.ParseInt(r.URL.Query().Get("size"), 10, 64)
	offset, err2 := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err1 != nil || err2 != nil || offset < 0 || offset > size {
		writeError(w, http.StatusBadRequest, "size and offset query parameters are required")
		return
	}
	complete, err := s.c.Blobs().Write(sha, size, offset, r.Body)
	var oe *blobstore.OffsetError
	switch {
	case errors.As(err, &oe):
		writeJSON(w, http.StatusConflict, map[string]any{"error": oe.Error(), "offset": oe.Have})
		return
	case errors.Is(err, blobstore.ErrInvalid), errors.Is(err, blobstore.ErrHashMismatch):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_, have := s.c.Blobs().Status(sha)
	writeJSON(w, http.StatusOK, map[string]any{"complete": complete, "offset": have})
}

func (s *Server) blobPost(w http.ResponseWriter, r *http.Request) {
	sha, size, err := s.c.Blobs().Put(r.Body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"sha256": sha, "size": size})
}

func (s *Server) taskOutputs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.c.Store().GetTask(id); err != nil {
		writeErr(w, err)
		return
	}
	outs, err := s.c.Store().TaskOutputs(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(outs))
}

func (s *Server) taskFile(w http.ResponseWriter, r *http.Request) {
	id, rel := r.PathValue("id"), r.PathValue("path")
	outs, err := s.c.Store().TaskOutputs(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	for _, o := range outs {
		if o.Path != rel {
			continue
		}
		f, err := s.c.Blobs().Open(o.SHA256)
		if err != nil {
			writeError(w, http.StatusGone, "file content is no longer stored")
			return
		}
		defer f.Close()
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path.Base(o.Path)))
		w.Header().Set("X-Content-SHA256", o.SHA256)
		// ServeContent handles Range requests, so downloads can resume.
		http.ServeContent(w, r, path.Base(o.Path), time.Time{}, f)
		return
	}
	writeError(w, http.StatusNotFound, "no such output")
}

func (s *Server) jobOutputs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.c.Store().GetJob(id); err != nil {
		writeErr(w, err)
		return
	}
	outs, err := s.c.Store().JobOutputs(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(outs))
}

// jobOutputsZip streams all outputs of a job as one zip. In array jobs
// each task's files go into a folder named after its index.
func (s *Server) jobOutputsZip(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, err := s.c.Store().GetJob(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	outs, err := s.c.Store().JobOutputs(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", id+"-outputs.zip"))
	zw := zip.NewWriter(w)
	for _, o := range outs {
		name := o.Path
		if job.TaskCount > 1 {
			name = fmt.Sprintf("%d/%s", o.Index, o.Path)
		}
		f, err := s.c.Blobs().Open(o.SHA256)
		if err != nil {
			continue
		}
		hdr := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Now()}
		hdr.SetMode(0o644)
		dst, err := zw.CreateHeader(hdr)
		if err == nil {
			_, err = io.Copy(dst, f)
		}
		f.Close()
		if err != nil {
			return // client went away; the response is already partly sent
		}
	}
	_ = zw.Close()
}

func nonNil(o []*store.OutputFile) []*store.OutputFile {
	if o == nil {
		return []*store.OutputFile{}
	}
	return o
}
