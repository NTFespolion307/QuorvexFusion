//go:build linux

package worker

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/NTFespolion307/QuorvexFusion/internal/controller"
	"github.com/NTFespolion307/QuorvexFusion/internal/store"
)

func upload(t *testing.T, tc *testCluster, content string) (string, int64) {
	t.Helper()
	sha, size, err := tc.c.Blobs().Put(strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	return sha, size
}

func readBlob(t *testing.T, tc *testCluster, sha string) string {
	t.Helper()
	f, err := tc.c.Blobs().Open(sha)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	return string(b)
}

// TestInputsAndOutputs: inputs (a file and a "folder") are placed in each
// task's working directory, outputs come back, and a second job with the
// same inputs reuses the worker's cache.
func TestInputsAndOutputs(t *testing.T) {
	tc := startController(t, 15)
	w1 := tc.newWorker(t, tc.joinToken, tc.fp)
	w2 := tc.newWorker(t, tc.joinToken, tc.fp)
	w1.opts.Name, w2.opts.Name = "w1", "w2"
	stop1, _ := runWorker(w1)
	defer stop1()
	stop2, _ := runWorker(w2)
	defer stop2()
	waitFor(t, 15*time.Second, "workers online", func() bool { p, _ := tc.c.Pool(); return p.NodesOnline == 2 })

	big := strings.Repeat("0123456789abcdef", 1<<16) // 1 MiB: several transfer chunks
	shaBig, sizeBig := upload(t, tc, big)
	shaA, sizeA := upload(t, tc, "alpha\n")
	shaS, sizeS := upload(t, tc, "#!/bin/sh\necho script ran with $1\n")
	inputs := []controller.InputSpec{
		{Path: "data/big.bin", SHA256: shaBig, Size: sizeBig},
		{Path: "data/sub/a.txt", SHA256: shaA, Size: sizeA},
		{Path: "run.sh", SHA256: shaS, Size: sizeS, Mode: 0o755},
	}
	job, err := tc.c.SubmitJob(&controller.JobSpec{
		Command: `set -e
./run.sh {i}
mkdir -p out/deep
sha256sum data/big.bin | cut -c1-64 > out/big.sha
cat data/sub/a.txt > out/deep/copy-{i}.txt
echo not collected > ignored.txt
# inputs are read-only links into the cache
if echo x >> data/big.bin 2>/dev/null; then echo writable; fi`,
		Array: "1-2", CPUs: 0.5, Inputs: inputs, Outputs: []string{"out/**", "missing/*"},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 60*time.Second, "job done", func() bool {
		j, _ := tc.c.Store().GetJob(job.ID)
		return j.Counts.Succeeded+j.Counts.Failed == 2
	})
	j, _ := tc.c.Store().GetJob(job.ID)
	if j.Counts.Succeeded != 2 {
		tk, _ := tc.c.Store().GetTask(store.TaskID(job.ID, 1))
		log, _, _, _ := tc.c.ReadLog(tk.ID, 0, controller.LogCombined, 0, 4096)
		t.Fatalf("job counts %+v; task 1: %+v\n%s", j.Counts, tk, log)
	}

	outs, _ := tc.c.Store().JobOutputs(job.ID)
	if len(outs) != 4 {
		t.Fatalf("got %d outputs, want 4: %+v", len(outs), outs)
	}
	for _, o := range outs {
		switch {
		case o.Path == "out/big.sha":
			if strings.TrimSpace(readBlob(t, tc, o.SHA256)) != shaBig {
				t.Errorf("input arrived corrupted: worker computed %q", readBlob(t, tc, o.SHA256))
			}
		case strings.HasPrefix(o.Path, "out/deep/copy-"):
			if readBlob(t, tc, o.SHA256) != "alpha\n" {
				t.Errorf("%s = %q", o.Path, readBlob(t, tc, o.SHA256))
			}
		default:
			t.Errorf("unexpected output %s", o.Path)
		}
	}
	log, _, _, _ := tc.c.ReadLog(store.TaskID(job.ID, 2), 0, controller.LogStdout, 0, 4096)
	if !bytes.Contains(log, []byte("script ran with 2")) {
		t.Errorf("executable input did not run: %q", log)
	}
	if bytes.Contains(log, []byte("writable")) {
		t.Error("task could modify a cached input")
	}

	// The same inputs again: the nodes already hold them, so nothing is
	// downloaded, and the scheduler prefers nodes with the data.
	before := tc.c.DownloadCount()
	job2, err := tc.c.SubmitJob(&controller.JobSpec{Command: "cat data/sub/a.txt", Inputs: inputs, CPUs: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "second job done", func() bool {
		tk, _ := tc.c.Store().GetTask(store.TaskID(job2.ID, 0))
		return tk.State.Terminal()
	})
	if tk, _ := tc.c.Store().GetTask(store.TaskID(job2.ID, 0)); tk.State != store.TaskSucceeded {
		t.Fatalf("second job: %+v", tk)
	}
	if n := tc.c.DownloadCount() - before; n != 0 {
		t.Errorf("second job downloaded %d file(s); expected the cache to be used", n)
	}

	// Inputs that were never uploaded are refused at submission.
	_, err = tc.c.SubmitJob(&controller.JobSpec{Command: "true", Inputs: []controller.InputSpec{
		{Path: "x", SHA256: strings.Repeat("ab", 32), Size: 1}}})
	if err == nil || !strings.Contains(err.Error(), "not been uploaded") {
		t.Errorf("missing input accepted: %v", err)
	}
}
