package controller

import (
	"fmt"
	"strings"
	"time"

	"github.com/NTFespolion307/QuorvexFusion/internal/files"
	"github.com/NTFespolion307/QuorvexFusion/internal/store"
)

// The file library: files uploaded to the controller once (from the web UI
// or the CLI, from anywhere) and then used as job inputs any number of
// times. Library entries point at blobs, like job inputs, so identical
// content is stored once and is only freed when nothing uses it any more.

// AddLibraryFile records an uploaded blob under a library path.
func (c *Controller) AddLibraryFile(p, sha string) (*store.LibraryFile, error) {
	clean, err := files.CleanRel(p)
	if err != nil {
		return nil, err
	}
	size, ok := c.blobs.Size(sha)
	if !ok {
		return nil, fmt.Errorf("file content %s has not been uploaded", shortSHA(sha))
	}
	f := &store.LibraryFile{Path: clean, SHA256: sha, Size: size, UploadedAt: time.Now()}
	if err := c.store.PutLibraryFile(f); err != nil {
		return nil, err
	}
	c.events.publish("files")
	return f, nil
}

// DeleteLibraryPath removes a file or a folder from the library.
func (c *Controller) DeleteLibraryPath(p string) (int, error) {
	clean, err := files.CleanRel(strings.TrimSuffix(p, "/"))
	if err != nil {
		return 0, err
	}
	n, err := c.store.DeleteLibraryFiles(clean)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, store.ErrNotFound
	}
	c.events.publish("files")
	go c.collectGarbage()
	return n, nil
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
