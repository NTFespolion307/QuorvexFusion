package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/NTFespolion307/QuorvexFusion/internal/clusterpb"
	"github.com/NTFespolion307/QuorvexFusion/internal/files"
)

// Moving files for tasks: inputs into the working directory before the
// process starts, outputs back to the controller after it ends.

const (
	uploadChunk    = 256 << 10
	maxOutputFiles = 10000
)

// progress reports transfer progress to the controller about once a
// second (and once at the end).
type progress struct {
	r       *Runner
	attempt string
	phase   string
	total   int64

	mu       sync.Mutex
	done     int64
	lastSent time.Time
}

func (p *progress) add(n int64) {
	p.mu.Lock()
	p.done += n
	due := time.Since(p.lastSent) >= time.Second || p.done >= p.total
	if due {
		p.lastSent = time.Now()
	}
	done := p.done
	p.mu.Unlock()
	if due {
		p.r.sendMsg(p.r.currentGen(), &pb.WorkerMessage{Msg: &pb.WorkerMessage_Progress{Progress: &pb.TaskProgress{
			AttemptId: p.attempt, Phase: p.phase, DoneBytes: done, TotalBytes: p.total,
		}}})
	}
}

// countingWriter reports bytes written to a progress tracker.
type countingWriter struct {
	w io.Writer
	p *progress
}

func (c countingWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.p.add(int64(n))
	return n, err
}

// stageInputs places every input in the task's working directory.
func (r *Runner) stageInputs(a *attempt, work string) error {
	inputs := a.assign.Spec.Inputs
	if len(inputs) == 0 {
		return nil
	}
	var total int64
	for _, in := range inputs {
		if in.SharedPath == "" {
			total += in.Size
		}
	}
	prog := &progress{r: r, attempt: a.id, phase: "download", total: total}

	for _, in := range inputs {
		dst, err := files.Join(work, in.Path)
		if err != nil {
			return err
		}
		if err := r.mkdirFor(work, dst); err != nil {
			return err
		}
		if in.SharedPath != "" {
			if err := r.linkShared(in, dst); err != nil {
				return err
			}
			continue
		}
		// Count bytes already cached as done, so progress reflects the work left.
		var counted int64
		fetch := func(ctx context.Context, offset int64, w io.Writer) error {
			if counted == 0 && offset > 0 {
				prog.add(offset)
				counted = offset
			}
			return r.download(ctx, in.Sha256, offset, countingWriter{w, prog})
		}
		cached, err := r.cache.ensure(a.stop, in.Sha256, in.Size, fetch)
		if err != nil {
			return fmt.Errorf("%s: %w", in.Path, err)
		}
		if counted == 0 {
			prog.add(in.Size) // was already in the cache
		}
		if err := r.place(cached, dst, in.Mode); err != nil {
			return fmt.Errorf("%s: %w", in.Path, err)
		}
	}
	return nil
}

// mkdirFor creates the parent directories of dst inside work, owned by the
// task user.
func (r *Runner) mkdirFor(work, dst string) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if r.user == nil {
		return nil
	}
	for d := dir; d != work && len(d) > len(work); d = filepath.Dir(d) {
		if err := chownTo(d, r.user); err != nil {
			return err
		}
	}
	return nil
}

// place puts a cached input at dst. Plain files are hard-linked (instant,
// no extra disk space, read-only); files that must be executable (scripts)
// get their own copy with the requested mode.
func (r *Runner) place(cached, dst string, mode uint32) error {
	_ = os.Remove(dst)
	if mode&0o111 == 0 {
		if err := os.Link(cached, dst); err == nil {
			return nil
		}
	}
	src, err := os.Open(cached)
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(mode&0o777))
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if r.user != nil {
		return chownTo(dst, r.user)
	}
	return nil
}

// linkShared links an input from the node's shared storage.
func (r *Runner) linkShared(in *pb.InputFile, dst string) error {
	if r.opts.SharedStorage == "" {
		return fmt.Errorf("%s: this node has no shared storage", in.Path)
	}
	src, err := files.Join(r.opts.SharedStorage, in.SharedPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("%s: not found in shared storage (%s)", in.Path, src)
	}
	_ = os.Remove(dst)
	return os.Symlink(src, dst)
}

// download streams one stored file from the controller, from offset.
func (r *Runner) download(ctx context.Context, sha string, offset int64, w io.Writer) error {
	client, _, err := r.connection(ctx)
	if err != nil {
		return err
	}
	stream, err := client.Download(ctx, &pb.DownloadRequest{Sha256: sha, Offset: offset})
	if err != nil {
		return transferErr(err)
	}
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return transferErr(err)
		}
		if _, err := w.Write(chunk.Data); err != nil {
			return err
		}
	}
}

// transferErr marks errors that retrying cannot fix.
func transferErr(err error) error {
	switch status.Code(err) {
	case codes.NotFound, codes.PermissionDenied, codes.InvalidArgument:
		return errPermanent{err}
	}
	return err
}

// collectOutputs hashes the output files of a finished task and records
// them in its result (they are uploaded before the result is sent).
func (r *Runner) collectOutputs(a *attempt, work string, res *pb.TaskResult) error {
	patterns := a.assign.Spec.Outputs
	if len(patterns) == 0 {
		return nil
	}
	rels, err := files.CollectOutputs(work, patterns, maxOutputFiles)
	if err != nil {
		return err
	}
	for _, rel := range rels {
		full := filepath.Join(work, filepath.FromSlash(rel))
		sha, size, err := files.HashFile(full)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		var mode uint32 = 0o644
		if st, err := os.Stat(full); err == nil {
			mode = uint32(st.Mode().Perm())
		}
		res.Outputs = append(res.Outputs, &pb.OutputFile{Path: rel, Sha256: sha, Size: size, Mode: mode})
	}
	return nil
}

// uploadOutputs sends a finished task's output files to the controller,
// resuming partial uploads. It is called (again) after every reconnect
// until the result is acknowledged; finished files are skipped quickly.
func (r *Runner) uploadOutputs(a *attempt, res *pb.TaskResult, gen int) error {
	if len(res.Outputs) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	defer cancel()
	r.mu.Lock()
	client, cur := r.client, r.gen
	r.mu.Unlock()
	if client == nil || cur != gen {
		return errors.New("not connected")
	}
	var total int64
	for _, o := range res.Outputs {
		total += o.Size
	}
	prog := &progress{r: r, attempt: a.id, phase: "upload", total: total}
	work := filepath.Join(a.dir, "work")
	for _, o := range res.Outputs {
		st, err := client.UploadStatus(ctx, &pb.UploadStatusRequest{Sha256: o.Sha256, Size: o.Size})
		if err != nil {
			return err
		}
		if st.Complete {
			prog.add(o.Size)
			continue
		}
		prog.add(st.Offset)
		if err := r.uploadFile(ctx, client, filepath.Join(work, filepath.FromSlash(o.Path)), o, st.Offset, prog); err != nil {
			return fmt.Errorf("%s: %w", o.Path, err)
		}
	}
	return nil
}

func (r *Runner) uploadFile(ctx context.Context, client pb.NodeServiceClient, path string, o *pb.OutputFile, offset int64, prog *progress) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	stream, err := client.Upload(ctx)
	if err != nil {
		return err
	}
	buf := make([]byte, uploadChunk)
	first := true
	for {
		n, rerr := f.Read(buf)
		if n > 0 || first {
			chunk := &pb.UploadChunk{Data: buf[:n]}
			if first {
				chunk.Sha256, chunk.Size, chunk.Offset = o.Sha256, o.Size, offset
				first = false
			}
			if err := stream.Send(chunk); err != nil {
				break // the real error comes from CloseAndRecv
			}
			prog.add(int64(n))
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return err
	}
	if !resp.Complete {
		return errors.New("controller did not receive the whole file")
	}
	return nil
}
