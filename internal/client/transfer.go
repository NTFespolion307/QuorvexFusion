package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Resumable file transfer for the CLI. Files are addressed by SHA-256;
// both directions continue where an interrupted transfer stopped.

// UploadFile makes sure the controller holds the file at path (whose
// SHA-256 and size are known), uploading only the missing part. progress
// is called with the number of bytes that are done (including any the
// controller already had).
func (c *Client) UploadFile(ctx context.Context, path, sha string, size int64, progress func(int64)) error {
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		offset, done, err := c.blobOffset(ctx, sha)
		if err != nil {
			lastErr = err
			continue
		}
		if done {
			progress(size)
			return nil
		}
		progress(offset)
		lastErr = c.putFrom(ctx, path, sha, size, offset, progress)
		if lastErr == nil {
			progress(size)
			return nil
		}
		var api *APIError
		if errors.As(lastErr, &api) && api.Status == http.StatusBadRequest {
			return lastErr // e.g. hash mismatch: the file changed while uploading
		}
	}
	return fmt.Errorf("upload of %s failed: %w", filepath.Base(path), lastErr)
}

// blobOffset asks how much of a file the controller has.
func (c *Client) blobOffset(ctx context.Context, sha string) (offset int64, complete bool, err error) {
	resp, err := c.Stream(ctx, "HEAD", "/api/v1/blobs/"+sha, nil, 0, nil, false)
	if err != nil {
		return 0, false, err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return 0, true, nil
	case http.StatusNotFound:
		off, _ := strconv.ParseInt(resp.Header.Get("X-Blob-Offset"), 10, 64)
		return off, false, nil
	}
	return 0, false, &APIError{Status: resp.StatusCode, Message: resp.Status}
}

type progressReader struct {
	r    io.Reader
	done int64
	fn   func(int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	p.fn(p.done)
	return n, err
}

func (c *Client) putFrom(ctx context.Context, path, sha string, size, offset int64, progress func(int64)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	body := &progressReader{r: io.LimitReader(f, size-offset), done: offset, fn: progress}
	q := fmt.Sprintf("/api/v1/blobs/%s?size=%d&offset=%d", sha, size, offset)
	resp, err := c.Stream(ctx, "PUT", q, body, size-offset, nil, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		Complete bool `json:"complete"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if !out.Complete {
		return errors.New("controller did not receive the whole file")
	}
	return nil
}

// Download fetches a file from an API path into dest, resuming from a
// leftover dest+".part" and verifying the SHA-256 at the end. A dest that
// already has the right content is left alone.
func (c *Client) Download(ctx context.Context, apiPath, dest, sha string, size int64, progress func(int64)) error {
	if got, n, err := hashFile(dest); err == nil && got == sha && n == size {
		progress(size)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	part := dest + ".part"
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		lastErr = c.downloadPart(ctx, apiPath, part, size, progress)
		if lastErr != nil {
			continue
		}
		got, _, err := hashFile(part)
		if err != nil {
			return err
		}
		if got != sha {
			os.Remove(part)
			lastErr = errors.New("downloaded data does not match its sha256")
			continue
		}
		return os.Rename(part, dest)
	}
	return lastErr
}

func (c *Client) downloadPart(ctx context.Context, apiPath, part string, size int64, progress func(int64)) error {
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	offset := st.Size()
	progress(offset)
	if offset >= size {
		return nil
	}
	hdr := http.Header{}
	hdr.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	resp, err := c.Stream(ctx, "GET", apiPath, nil, 0, hdr, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent && offset > 0 {
		// The server ignored the range: start over.
		if err := f.Truncate(0); err != nil {
			return err
		}
		offset = 0
	}
	_, err = io.Copy(f, &progressReader{r: resp.Body, done: offset, fn: progress})
	return err
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
