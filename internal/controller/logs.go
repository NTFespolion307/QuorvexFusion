package controller

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	pb "github.com/NTFespolion307/QuorvexFusion/internal/clusterpb"
)

// logStore keeps task output on the controller's disk:
//
//	logs/<task id>/<attempt number>.stdout
//	logs/<task id>/<attempt number>.stderr
//	logs/<task id>/<attempt number>.combined   both, interleaved as received
//
// Workers send chunks tagged with their byte offset within the stream.
// Appending is idempotent: bytes we already have are skipped, so a worker
// can safely resend after a reconnect.
type logStore struct {
	dir     string
	maxSize int64 // per stream; further output is dropped with a notice
	mu      sync.Mutex
}

func newLogStore(dir string) *logStore {
	return &logStore{dir: dir, maxSize: 256 << 20}
}

// LogStream names one of an attempt's log files.
type LogStream string

const (
	LogCombined LogStream = "combined"
	LogStdout   LogStream = "stdout"
	LogStderr   LogStream = "stderr"
)

func (l *logStore) path(taskID string, attempt int, stream LogStream) string {
	return filepath.Join(l.dir, taskID, fmt.Sprintf("%d.%s", attempt, stream))
}

func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// Sizes reports how many bytes of each stream we hold for an attempt.
func (l *logStore) Sizes(taskID string, attempt int) *pb.LogOffsets {
	l.mu.Lock()
	defer l.mu.Unlock()
	return &pb.LogOffsets{
		Stdout: fileSize(l.path(taskID, attempt, LogStdout)),
		Stderr: fileSize(l.path(taskID, attempt, LogStderr)),
	}
}

// Append stores a chunk. Bytes before the current end of the stream are
// already stored and are skipped.
func (l *logStore) Append(taskID string, attempt int, chunk *pb.LogChunk) error {
	stream := LogStdout
	if chunk.Stream == pb.Stream_STDERR {
		stream = LogStderr
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	p := l.path(taskID, attempt, stream)
	have := fileSize(p)
	data := chunk.Data
	if skip := have - chunk.Offset; skip > 0 {
		if skip >= int64(len(data)) {
			return nil // all duplicate
		}
		data = data[skip:]
	}
	// A gap (offset beyond what we have) should not happen since workers
	// resume from our offsets; if it does, we just append what we got.
	if have >= l.maxSize {
		return nil
	}
	if have+int64(len(data)) > l.maxSize {
		data = append(data[:l.maxSize-have:l.maxSize-have], []byte("\n[log truncated: size limit reached]\n")...)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if err := appendFile(p, data); err != nil {
		return err
	}
	return appendFile(l.path(taskID, attempt, LogCombined), data)
}

func appendFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Read returns up to max bytes of a stream starting at offset, plus the
// stream's total size.
func (l *logStore) Read(taskID string, attempt int, stream LogStream, offset, max int64) ([]byte, int64, error) {
	f, err := os.Open(l.path(taskID, attempt, stream))
	if os.IsNotExist(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := st.Size()
	if offset >= size {
		return nil, size, nil
	}
	n := min(size-offset, max)
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, offset); err != nil && err != io.EOF {
		return nil, 0, err
	}
	return buf, size, nil
}

// RemoveTask deletes all logs of a task.
func (l *logStore) RemoveTask(taskID string) error {
	return os.RemoveAll(filepath.Join(l.dir, taskID))
}
