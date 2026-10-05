package main

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/NTFespolion307/QuorvexFusion/internal/controller"
	"github.com/NTFespolion307/QuorvexFusion/internal/files"
	"github.com/NTFespolion307/QuorvexFusion/internal/store"
)

// --- progress line on stderr -------------------------------------------

// progressLine prints "label: 45% (1.2 GiB / 2.6 GiB)" on one terminal line.
type progressLine struct {
	label string
	total int64

	mu    sync.Mutex
	parts map[string]int64 // per file, so retries don't double count
	last  time.Time
}

func newProgress(label string, total int64) *progressLine {
	return &progressLine{label: label, total: total, parts: map[string]int64{}}
}

func (p *progressLine) set(key string, done int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.parts[key] = done
	// Live updates only on a terminal; logs and pipes get the final line.
	if !stderrIsTerminal || time.Since(p.last) < 200*time.Millisecond {
		return
	}
	p.last = time.Now()
	p.print()
}

func (p *progressLine) print() {
	var done int64
	for _, n := range p.parts {
		done += n
	}
	pct := 100.0
	if p.total > 0 {
		pct = 100 * float64(done) / float64(p.total)
	}
	fmt.Fprintf(os.Stderr, "\r%s: %3.0f%% (%s / %s)   ", p.label, pct, humanBytes(uint64(done)), humanBytes(uint64(p.total)))
}

func (p *progressLine) finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.print()
	fmt.Fprintln(os.Stderr)
}

var stderrIsTerminal = term.IsTerminal(int(os.Stderr.Fd()))

// --- inputs ----------------------------------------------------------------

// splitSrcDest parses SRC[:DEST], keeping Windows drive letters ("C:\x") intact.
func splitSrcDest(s string) (src, dest string) {
	start := 0
	if runtime.GOOS == "windows" && len(s) > 2 && s[1] == ':' {
		start = 2
	}
	if i := strings.LastIndex(s[start:], ":"); i >= 0 {
		return s[:start+i], s[start+i+1:]
	}
	return s, ""
}

type localInput struct {
	local string // path on this computer
	spec  controller.InputSpec
}

// collectInputs expands --input arguments (files and folders) into files.
func collectInputs(args []string) ([]localInput, error) {
	var out []localInput
	for _, arg := range args {
		src, dest := splitSrcDest(arg)
		info, err := os.Stat(src)
		if err != nil {
			return nil, fmt.Errorf("--input %s: %w", src, err)
		}
		if dest == "" {
			dest = filepath.Base(filepath.Clean(src))
		}
		if !info.IsDir() {
			out = append(out, localInput{local: src, spec: controller.InputSpec{Path: filepath.ToSlash(dest), Mode: fileMode(info)}})
			continue
		}
		err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.Type().IsRegular() {
				return nil // folders are walked; links and special files skipped
			}
			rel, _ := filepath.Rel(src, p)
			info, err := d.Info()
			if err != nil {
				return err
			}
			out = append(out, localInput{local: p, spec: controller.InputSpec{
				Path: path.Join(filepath.ToSlash(dest), filepath.ToSlash(rel)), Mode: fileMode(info)}})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("--input %s: %w", src, err)
		}
	}
	for i := range out {
		p, err := files.CleanRel(out[i].spec.Path)
		if err != nil {
			return nil, fmt.Errorf("--input destination: %w", err)
		}
		out[i].spec.Path = p
	}
	return out, nil
}

func fileMode(info os.FileInfo) uint32 {
	if info.Mode().Perm()&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

// scriptCommand returns how to run an uploaded script: directly if it has
// a "#!" line, else with an interpreter chosen by its extension.
func scriptCommand(local, name string) (string, error) {
	f, err := os.Open(local)
	if err != nil {
		return "", err
	}
	defer f.Close()
	first, _ := bufio.NewReader(f).ReadString('\n')
	if strings.HasPrefix(first, "#!") {
		return "./" + shellQuote(name), nil
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".py":
		return "python3 " + shellQuote(name), nil
	case ".sh", ".bash":
		return "bash " + shellQuote(name), nil
	case ".r":
		return "Rscript " + shellQuote(name), nil
	case ".js":
		return "node " + shellQuote(name), nil
	case ".pl":
		return "perl " + shellQuote(name), nil
	}
	return "./" + shellQuote(name), nil
}

// uploadInputs hashes local inputs, uploads whatever the controller lacks,
// and returns the input specs for the job.
func uploadInputs(inputs []localInput) ([]controller.InputSpec, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	c, err := apiClient()
	if err != nil {
		return nil, err
	}
	var total int64
	hashing := newProgress(fmt.Sprintf("Hashing %d input file(s)", len(inputs)), 0)
	for i := range inputs {
		sha, size, err := files.HashFile(inputs[i].local)
		if err != nil {
			return nil, err
		}
		inputs[i].spec.SHA256, inputs[i].spec.Size = sha, size
		total += size
		hashing.total = total
		hashing.set(sha, size)
	}
	hashing.finish()

	// Identical files are uploaded once.
	unique := map[string]localInput{}
	var uniqueBytes int64
	for _, in := range inputs {
		if _, ok := unique[in.spec.SHA256]; !ok {
			unique[in.spec.SHA256] = in
			uniqueBytes += in.spec.Size
		}
	}
	prog := newProgress("Uploading inputs", uniqueBytes)
	ctx := context.Background()
	for sha, in := range unique {
		if err := c.UploadFile(ctx, in.local, sha, in.spec.Size, func(n int64) { prog.set(sha, n) }); err != nil {
			fmt.Fprintln(os.Stderr)
			return nil, err
		}
	}
	prog.finish()

	specs := make([]controller.InputSpec, len(inputs))
	for i, in := range inputs {
		specs[i] = in.spec
	}
	return specs, nil
}

// --- outputs ---------------------------------------------------------------

func outputsCmd() *cobra.Command {
	var dir string
	var list bool
	cmd := &cobra.Command{
		Use:   "outputs <job-id | task-id>",
		Short: "Download the output files of a job or task",
		Long: `Download the files a job declared with --output. For array jobs, each
task's files go into a folder named after its index. Interrupted downloads
resume, and files already downloaded are skipped.`,
		Example: `  cluster outputs j1a2b3c4d               # into ./j1a2b3c4d/
  cluster outputs j1a2b3c4d -o renders
  cluster outputs j1a2b3c4d.7 --list`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]
			var outs []*store.OutputFile
			perTaskDirs := false
			if isTaskID(ref) {
				if err := call("GET", "/api/v1/tasks/"+url.PathEscape(ref)+"/outputs", nil, &outs); err != nil {
					return err
				}
			} else {
				var j controller.JobView
				if err := call("GET", "/api/v1/jobs/"+url.PathEscape(ref), nil, &j); err != nil {
					return err
				}
				if err := call("GET", "/api/v1/jobs/"+url.PathEscape(ref)+"/outputs", nil, &outs); err != nil {
					return err
				}
				perTaskDirs = j.TaskCount > 1
			}
			if globalFlags.json {
				return printJSON(outs)
			}
			if len(outs) == 0 {
				fmt.Fprintln(os.Stderr, "No output files (declare them with: cluster submit --output 'pattern' ...).")
				return nil
			}
			if list {
				t := newTable()
				fmt.Fprintln(t, "TASK\tPATH\tSIZE\tSHA256")
				for _, o := range outs {
					fmt.Fprintf(t, "%s\t%s\t%s\t%s\n", o.TaskID, o.Path, humanBytes(uint64(o.Size)), o.SHA256[:16])
				}
				return t.Flush()
			}
			if dir == "" {
				dir = ref
			}
			c, err := apiClient()
			if err != nil {
				return err
			}
			var total int64
			for _, o := range outs {
				total += o.Size
			}
			prog := newProgress(fmt.Sprintf("Downloading %d file(s)", len(outs)), total)
			for _, o := range outs {
				rel := o.Path
				if perTaskDirs {
					rel = fmt.Sprintf("%d/%s", o.Index, o.Path)
				}
				dest, err := files.Join(dir, rel)
				if err != nil {
					return err
				}
				key := o.TaskID + "/" + o.Path
				apiPath := "/api/v1/tasks/" + url.PathEscape(o.TaskID) + "/files/" + escapePath(o.Path)
				if err := c.Download(context.Background(), apiPath, dest, o.SHA256, o.Size, func(n int64) { prog.set(key, n) }); err != nil {
					fmt.Fprintln(os.Stderr)
					return fmt.Errorf("%s: %w", rel, err)
				}
			}
			prog.finish()
			fmt.Printf("Saved to %s\n", dir)
			return nil
		},
	}
	cmd.Flags().StringVarP(&dir, "dir", "o", "", "directory to save into (default: the job or task ID)")
	cmd.Flags().BoolVar(&list, "list", false, "only list the files")
	return cmd
}

// escapePath escapes each segment of a slash-separated path for a URL.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}
