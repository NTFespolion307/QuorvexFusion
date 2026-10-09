package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/NTFespolion307/QuorvexFusion/internal/blobstore"
	pb "github.com/NTFespolion307/QuorvexFusion/internal/clusterpb"
	"github.com/NTFespolion307/QuorvexFusion/internal/store"
)

// File transfer between the controller and workers. Content lives in the
// blob store (by SHA-256); workers download task inputs and upload task
// outputs over gRPC streams next to their control stream.

const transferChunk = 256 << 10

// Download streams an input file to a worker, from the requested offset
// (so interrupted downloads resume).
func (ns *nodeServer) Download(req *pb.DownloadRequest, stream pb.NodeService_DownloadServer) error {
	c := ns.c
	nodeID := nodeIDFrom(stream.Context())
	if !c.nodeMayDownload(nodeID, req.Sha256) {
		return status.Error(codes.PermissionDenied, "this file is not an input of a task on this node")
	}
	f, err := c.blobs.Open(req.Sha256)
	if err != nil {
		return status.Error(codes.NotFound, "file not found on the controller")
	}
	defer f.Close()
	if _, err := f.Seek(req.Offset, io.SeekStart); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	buf := make([]byte, transferChunk)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if serr := stream.Send(&pb.FileChunk{Data: buf[:n]}); serr != nil {
				return serr
			}
		}
		if errors.Is(err, io.EOF) {
			c.recordCached(nodeID, req.Sha256)
			c.downloads.Add(1)
			return nil
		}
		if err != nil {
			return status.Error(codes.Internal, err.Error())
		}
	}
}

// UploadStatus tells a worker where to resume an output upload.
func (ns *nodeServer) UploadStatus(ctx context.Context, req *pb.UploadStatusRequest) (*pb.UploadStatusResponse, error) {
	if !blobstore.ValidSHA(req.Sha256) {
		return nil, status.Error(codes.InvalidArgument, "invalid sha256")
	}
	complete, have := ns.c.blobs.Status(req.Sha256)
	return &pb.UploadStatusResponse{Complete: complete, Offset: have}, nil
}

// Upload receives an output file. The first chunk names the file, its size
// and the offset it starts at (from UploadStatus).
func (ns *nodeServer) Upload(stream pb.NodeService_UploadServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write(first.Data)
		for {
			chunk, err := stream.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					pw.Close()
				} else {
					pw.CloseWithError(err)
				}
				return
			}
			if _, err := pw.Write(chunk.Data); err != nil {
				return
			}
		}
	}()
	complete, err := ns.c.blobs.Write(first.Sha256, first.Size, first.Offset, pr)
	pr.Close()
	var oe *blobstore.OffsetError
	switch {
	case errors.As(err, &oe):
		return status.Error(codes.FailedPrecondition, oe.Error())
	case errors.Is(err, blobstore.ErrInvalid), errors.Is(err, blobstore.ErrHashMismatch):
		return status.Error(codes.InvalidArgument, err.Error())
	case err != nil:
		return status.Error(codes.Internal, err.Error())
	}
	return stream.SendAndClose(&pb.UploadResponse{Complete: complete})
}

// nodeMayDownload allows a node to fetch only inputs of tasks it runs.
func (c *Controller) nodeMayDownload(nodeID, sha string) bool {
	c.tm.mu.Lock()
	defer c.tm.mu.Unlock()
	for _, r := range c.tm.reservations {
		if r.nodeID != nodeID {
			continue
		}
		spec, err := c.jobSpecLocked(r.jobID)
		if err != nil {
			continue
		}
		for _, in := range spec.Inputs {
			if in.SHA256 == sha {
				return true
			}
		}
	}
	return false
}

// recordCached remembers that a node now holds a file, so the scheduler
// can prefer it for tasks with the same inputs.
func (c *Controller) recordCached(nodeID, sha string) {
	c.tm.mu.Lock()
	defer c.tm.mu.Unlock()
	m := c.tm.cached[nodeID]
	if m == nil {
		m = map[string]bool{}
		c.tm.cached[nodeID] = m
	}
	m[sha] = true
}

// preferredNodesLocked returns nodes that already hold at least half of a
// job's input bytes.
func (c *Controller) preferredNodesLocked(spec *JobSpec) map[string]bool {
	total := spec.inputBytes()
	if total == 0 {
		return nil
	}
	out := map[string]bool{}
	for node, have := range c.tm.cached {
		var n int64
		for _, in := range spec.Inputs {
			if in.SHA256 != "" && have[in.SHA256] {
				n += in.Size
			}
		}
		if 2*n >= total {
			out[node] = true
		}
	}
	return out
}

// handleProgress records transfer progress for display.
func (c *Controller) handleProgress(nodeID string, p *pb.TaskProgress) {
	c.tm.mu.Lock()
	r := c.tm.reservations[p.AttemptId]
	if r != nil && r.nodeID == nodeID {
		c.tm.progress[p.AttemptId] = p
	}
	c.tm.mu.Unlock()
	c.publishThrottled("jobs")
}

// checkInputsUploaded verifies that every uploaded input of a job exists.
func (c *Controller) checkInputsUploaded(spec *JobSpec) error {
	for i := range spec.Inputs {
		in := &spec.Inputs[i]
		if in.SHA256 == "" {
			continue
		}
		size, ok := c.blobs.Size(in.SHA256)
		if !ok {
			return fmt.Errorf("input %q has not been uploaded (sha256 %s)", in.Path, in.SHA256[:12])
		}
		in.Size = size
	}
	return nil
}

// acceptOutputs converts a worker's output list, keeping only files that
// really arrived (workers upload before sending the result).
func (c *Controller) acceptOutputs(res *pb.TaskResult) (out []store.OutputFile, missing int) {
	for _, f := range res.Outputs {
		if _, ok := c.blobs.Size(f.Sha256); !ok {
			missing++
			continue
		}
		out = append(out, store.OutputFile{Path: f.Path, SHA256: f.Sha256, Size: f.Size, Mode: f.Mode & 0o777})
	}
	return out, missing
}

// collectGarbage deletes stored files no job or output refers to any more.
// Files younger than two hours are kept: they may belong to a job that is
// being submitted right now.
func (c *Controller) collectGarbage() {
	specs, refs, err := c.store.ReferencedBlobs()
	if err != nil {
		c.log.Error("file cleanup", "err", err)
		return
	}
	if err := c.store.LibraryBlobs(refs); err != nil {
		c.log.Error("file cleanup", "err", err)
		return
	}
	for _, raw := range specs {
		var spec JobSpec
		if json.Unmarshal([]byte(raw), &spec) == nil {
			for _, in := range spec.Inputs {
				if in.SHA256 != "" {
					refs[in.SHA256] = true
				}
			}
		}
	}
	freed, err := c.blobs.GC(refs, 2*time.Hour)
	if err != nil {
		c.log.Error("file cleanup", "err", err)
	}
	if freed > 0 {
		c.log.Info("removed unused files", "bytes", freed)
	}
}

func (c *Controller) maintenanceLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.collectGarbage()
		}
	}
}

// DownloadCount is how many input downloads workers have completed.
func (c *Controller) DownloadCount() int64 { return c.downloads.Load() }

// Blobs gives the API access to stored files (uploads, output downloads).
func (c *Controller) Blobs() *blobstore.Store { return c.blobs }
