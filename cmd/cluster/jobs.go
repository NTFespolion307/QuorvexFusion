package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/NTFespolion307/QuorvexFusion/internal/controller"
	"github.com/NTFespolion307/QuorvexFusion/internal/store"
)

// --- helpers ---

var safeShellWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./{}-]+$`)

// shellQuote quotes s for /bin/sh unless it is plainly safe.
func shellQuote(s string) string {
	if safeShellWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// joinCommand turns CLI args into one shell command. A single argument is
// taken as shell code as-is (`cluster submit -- 'make && ./run'`); several
// arguments are quoted individually, like ssh does.
func joinCommand(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shellQuote(a)
	}
	return strings.Join(q, " ")
}

// parseBytes accepts sizes like 512M, 4G, 1.5GiB or plain bytes (binary units).
func parseBytes(s string) (uint64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, nil
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "B"), "I")
	mult := 1.0
	if n := len(s); n > 0 {
		switch s[n-1] {
		case 'K':
			mult = 1 << 10
		case 'M':
			mult = 1 << 20
		case 'G':
			mult = 1 << 30
		case 'T':
			mult = 1 << 40
		}
		if mult != 1 {
			s = s[:n-1]
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("bad size %q (use e.g. 512M or 4G)", s)
	}
	return uint64(v * mult), nil
}

func parseKV(list []string, what string) (map[string]string, error) {
	if len(list) == 0 {
		return nil, nil
	}
	m := map[string]string{}
	for _, kv := range list {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("%s %q: expected KEY=VALUE", what, kv)
		}
		m[k] = v
	}
	return m, nil
}

func duration(start, end *time.Time) string {
	if start == nil {
		return "-"
	}
	e := time.Now()
	if end != nil {
		e = *end
	}
	return e.Sub(*start).Round(100 * time.Millisecond).String()
}

func isTaskID(ref string) bool { return strings.Contains(ref, ".") }

// --- submit ---

func submitCmd() *cobra.Command {
	var (
		spec                 controller.JobSpec
		memory               string
		timeout              time.Duration
		env, require, prefer []string
		noEphemeral          bool
		noRemote             bool
		wait, follow         bool
		inputs, sharedInputs []string
		script               string
	)
	cmd := &cobra.Command{
		Use:   "submit [flags] -- command [args...]",
		Short: "Submit a job",
		Long: `Submit a job. The command runs with /bin/sh -c on whichever node the
scheduler picks. With --array, one task runs per index and {i} in the
command (and env values) is replaced by the index.

Files: --input copies files or folders from this computer into every
task's working directory (uploaded once, cached on the nodes); --output
names files the tasks produce, collected with 'cluster outputs <job>'.
--script uploads a script and runs it (the command line becomes its
arguments).`,
		Example: `  cluster submit --cpus 4 -- ./render.sh 12
  cluster submit --array 1-500 --cpus 1 -- 'python3 sim.py --seed {i}'
  cluster submit --script train.py --input data/ --output 'model/*' --gpus 1 -- --epochs 10
  cluster submit --array 1-2500:10 --input scene.blend --output 'frames/*' -- \
      'blender -b scene.blend -o //frames/f_#### -s {i} -e $(( {i} + 9 )) -a'
  cluster submit --image python:3.12-slim --input data/ --output 'out/*' -- python3 -c 'print(1)'
  cluster submit --image nvidia/cuda:12.4.1-base-ubuntu22.04 --gpus 1 -f -- nvidia-smi
  cluster submit -f -- uname -a`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if script == "" && len(args) == 0 && spec.Image == "" {
				return errors.New("give a command after --, a --script, or an --image")
			}
			local, err := collectInputs(inputs)
			if err != nil {
				return err
			}
			if script != "" {
				name := filepath.Base(script)
				run, err := scriptCommand(script, name)
				if err != nil {
					return err
				}
				local = append(local, localInput{local: script, spec: controller.InputSpec{Path: name, Mode: 0o755}})
				// With a script, every argument is passed to it literally.
				for _, a := range args {
					run += " " + shellQuote(a)
				}
				spec.Command = run
			} else {
				spec.Command = joinCommand(args)
			}
			if spec.Inputs, err = uploadInputs(local); err != nil {
				return err
			}
			for _, si := range sharedInputs {
				src, dest := splitSrcDest(si)
				if dest == "" {
					dest = path.Base(filepath.ToSlash(src))
				}
				spec.Inputs = append(spec.Inputs, controller.InputSpec{Path: dest, Shared: filepath.ToSlash(src)})
			}
			if spec.MemoryBytes, err = parseBytes(memory); err != nil {
				return err
			}
			spec.TimeoutSec = int64(timeout.Seconds())
			if spec.Env, err = parseKV(env, "--env"); err != nil {
				return err
			}
			if spec.Requires, err = parseKV(require, "--require"); err != nil {
				return err
			}
			if spec.Prefers, err = parseKV(prefer, "--prefer"); err != nil {
				return err
			}
			if noEphemeral {
				f := false
				spec.AllowEphemeral = &f
			}
			if noRemote {
				f := false
				spec.AllowRemote = &f
			}

			var job controller.JobView
			if err := call("POST", "/api/v1/jobs", &spec, &job); err != nil {
				return err
			}
			if globalFlags.json && !wait && !follow {
				return printJSON(job)
			}
			fmt.Fprintf(os.Stderr, "Submitted job %s (%d task(s))\n", job.ID, job.TaskCount)

			switch {
			case follow && job.TaskCount == 1:
				state, code, err := followTask(store.TaskID(job.ID, firstIndex(job.Spec.Array)), controller.LogCombined, 0, true)
				if err != nil {
					return err
				}
				return taskExit(state, code)
			case follow || wait:
				err := waitJob(job.ID)
				fmt.Fprintf(os.Stderr, "Output of every task: cluster logs %s\n", job.ID)
				return err
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&spec.Name, "name", "", "job name (default: the command)")
	f.Float64Var(&spec.CPUs, "cpus", 1, "CPU cores per task (fractions allowed)")
	f.StringVar(&memory, "memory", "", "memory per task, e.g. 512M or 4G (also enforced as a limit)")
	f.IntVar(&spec.GPUs, "gpus", 0, "GPUs per task")
	f.IntVar(&spec.Retries, "retries", 0, "retry a failed task up to this many times")
	f.DurationVar(&timeout, "timeout", 0, "kill a task attempt after this long, e.g. 30m")
	f.StringVar(&spec.Array, "array", "", "run many tasks: a count (16 = indices 1..16), a range 1-500, 0-99:10, or a list 1,5,9")
	f.IntVar(&spec.Priority, "priority", 0, "higher priority jobs are scheduled first")
	f.StringArrayVarP(&env, "env", "e", nil, "environment variable KEY=VALUE (repeatable)")
	f.StringArrayVar(&require, "require", nil, "only run on nodes with label KEY=VALUE (repeatable)")
	f.StringArrayVar(&prefer, "prefer", nil, "prefer nodes with label KEY=VALUE (repeatable)")
	f.BoolVar(&noEphemeral, "no-ephemeral", false, "never run on ephemeral (rented/cloud) nodes")
	f.BoolVar(&noRemote, "no-remote", false, "never run on remote nodes")
	f.StringArrayVarP(&inputs, "input", "i", nil, "file or folder to place in each task's working directory, SRC or SRC:DEST (repeatable)")
	f.StringArrayVar(&sharedInputs, "shared-input", nil, "path in the nodes' shared storage, linked without transfer: PATH or PATH:DEST (repeatable)")
	f.StringArrayVarP(&spec.Outputs, "output", "o", nil, "files to collect after each task, a glob like 'out/*.png' or 'results/**' (repeatable)")
	f.StringVar(&script, "script", "", "upload this script and run it; arguments after -- are passed to it")
	f.StringVar(&spec.Image, "image", "", "run each task in this Docker image (the command runs inside it; none = the image's default)")
	f.BoolVarP(&wait, "wait", "w", false, "wait until all tasks finish")
	f.BoolVarP(&follow, "follow", "f", false, "stream output (single-task jobs) or wait for the job")
	return cmd
}

func firstIndex(array string) int64 {
	idx, err := controller.ParseArray(array)
	if err != nil || len(idx) == 0 {
		return 0
	}
	return idx[0]
}

// taskExit makes the CLI exit like the task did, for scripting.
func taskExit(state store.TaskState, code int) error {
	switch state {
	case store.TaskSucceeded:
		return nil
	case store.TaskFailed:
		if code <= 0 || code > 255 {
			code = 1
		}
		return &exitError{code: code, err: errors.New("task failed")}
	default:
		return &exitError{code: 1, err: fmt.Errorf("task %s", state)}
	}
}

// waitJob polls until no task of the job is queued or running.
func waitJob(jobID string) error {
	last := ""
	for {
		var j controller.JobView
		if err := call("GET", "/api/v1/jobs/"+url.PathEscape(jobID), nil, &j); err != nil {
			return err
		}
		c := j.Counts
		line := fmt.Sprintf("%s: %d queued, %d running, %d succeeded, %d failed, %d canceled",
			j.ID, c.Queued, c.Running+c.Assigned, c.Succeeded, c.Failed, c.Canceled)
		if line != last {
			fmt.Fprintln(os.Stderr, line)
			last = line
		}
		if c.Queued+c.Running+c.Assigned == 0 {
			if c.Failed+c.Canceled > 0 {
				return &exitError{code: 1, err: fmt.Errorf("job %s finished with %d failed and %d canceled task(s)", j.ID, c.Failed, c.Canceled)}
			}
			return nil
		}
		time.Sleep(time.Second)
	}
}

// --- logs ---

// printJobLogs prints the output of every task of a job, each under a
// header naming the task, its state and node.
func printJobLogs(jobID string, stream controller.LogStream) error {
	nodes := map[string]string{}
	var nodeList []*controller.NodeView
	if err := call("GET", "/api/v1/nodes", nil, &nodeList); err == nil {
		for _, n := range nodeList {
			nodes[n.ID] = n.Name
		}
	}
	for offset := 0; ; offset += 1000 {
		var tasks []*controller.TaskView
		if err := call("GET", fmt.Sprintf("/api/v1/jobs/%s/tasks?limit=1000&offset=%d", url.PathEscape(jobID), offset), nil, &tasks); err != nil {
			return err
		}
		for _, t := range tasks {
			where := nodes[t.NodeID]
			if where == "" {
				where = t.NodeID
			}
			if where != "" {
				where = " on " + where
			}
			fmt.Printf("==> %s (%s%s) <==\n", t.ID, t.State, where)
			if t.Attempts > 0 {
				if _, _, err := followTask(t.ID, stream, 0, false); err != nil {
					return err
				}
			}
		}
		if len(tasks) < 1000 {
			return nil
		}
	}
}

// resolveTask maps a job ID of a single-task job to its task ID.
func resolveTask(ref string) (string, error) {
	if isTaskID(ref) {
		return ref, nil
	}
	var j controller.JobView
	if err := call("GET", "/api/v1/jobs/"+url.PathEscape(ref), nil, &j); err != nil {
		return "", err
	}
	if j.TaskCount != 1 {
		return "", fmt.Errorf("job %s has %d tasks; pick one, e.g. %s", j.ID, j.TaskCount, store.TaskID(j.ID, firstIndex(j.Spec.Array)))
	}
	return store.TaskID(j.ID, firstIndex(j.Spec.Array)), nil
}

// followTask prints a task's log. With follow, it keeps polling until the
// task finishes, switching to new attempts when the task is retried. It
// returns the task's final state and exit code.
func followTask(taskID string, stream controller.LogStream, attempt int, follow bool) (store.TaskState, int, error) {
	c, err := apiClient()
	if err != nil {
		return "", 0, err
	}
	var offset int64
	cur := attempt
	for {
		q := url.Values{"stream": {string(stream)}, "offset": {fmt.Sprint(offset)}}
		if attempt > 0 {
			q.Set("attempt", fmt.Sprint(attempt))
		}
		body, hdr, err := c.Raw(context.Background(), "GET", "/api/v1/tasks/"+url.PathEscape(taskID)+"/logs?"+q.Encode())
		if err != nil {
			return "", 0, err
		}
		got, _ := strconv.Atoi(hdr.Get("X-Log-Attempt"))
		if attempt == 0 && got != cur && cur != 0 {
			// The task was retried: continue with the new attempt's log.
			fmt.Fprintf(os.Stderr, "\n--- attempt %d ---\n", got)
			offset = 0
			cur = got
			continue
		}
		cur = got
		os.Stdout.Write(body)
		offset += int64(len(body))
		size, _ := strconv.ParseInt(hdr.Get("X-Log-Size"), 10, 64)
		state := store.TaskState(hdr.Get("X-Task-State"))
		if offset < size {
			continue // more already available
		}
		if !follow || state.Terminal() {
			var t controller.TaskView
			if err := call("GET", "/api/v1/tasks/"+url.PathEscape(taskID), nil, &t); err != nil {
				return state, 0, err
			}
			code := 0
			if t.ExitCode != nil {
				code = *t.ExitCode
			}
			if follow && t.Error != "" && t.State != store.TaskSucceeded {
				fmt.Fprintf(os.Stderr, "task %s %s: %s\n", taskID, t.State, t.Error)
			}
			return t.State, code, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func logsCmd() *cobra.Command {
	var follow bool
	var stream string
	var attempt int
	cmd := &cobra.Command{
		Use:   "logs <task-id | job-id>",
		Short: "Print the output of a task, or of every task of a job",
		Example: `  cluster logs j1a2b3c4d          # every task of the job, one after another
  cluster logs j1a2b3c4d.7        # one task of an array job
  cluster logs -f j1a2b3c4d.17 --stream stderr`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]
			if !isTaskID(ref) {
				var j controller.JobView
				if err := call("GET", "/api/v1/jobs/"+url.PathEscape(ref), nil, &j); err != nil {
					return err
				}
				if j.TaskCount > 1 {
					if follow {
						return fmt.Errorf("-f follows one task; pick one, e.g. %s", store.TaskID(j.ID, firstIndex(j.Spec.Array)))
					}
					return printJobLogs(j.ID, controller.LogStream(stream))
				}
				ref = store.TaskID(j.ID, firstIndex(j.Spec.Array))
			}
			_, _, err := followTask(ref, controller.LogStream(stream), attempt, follow)
			return err
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new output until the task finishes")
	cmd.Flags().StringVar(&stream, "stream", "combined", "combined, stdout or stderr")
	cmd.Flags().IntVar(&attempt, "attempt", 0, "attempt number (default: latest)")
	return cmd
}

// --- cancel ---

func cancelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <job-id | task-id>...",
		Short: "Cancel jobs or individual tasks",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, ref := range args {
				path := "/api/v1/jobs/" + url.PathEscape(ref) + "/cancel"
				if isTaskID(ref) {
					path = "/api/v1/tasks/" + url.PathEscape(ref) + "/cancel"
				}
				if err := call("POST", path, nil, nil); err != nil {
					return fmt.Errorf("%s: %w", ref, err)
				}
				fmt.Printf("%s: canceled\n", ref)
			}
			return nil
		},
	}
}

// --- jobs ---

func jobsCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:     "jobs",
		Aliases: []string{"job"},
		Short:   "List jobs, show their tasks, delete finished jobs",
		RunE: func(cmd *cobra.Command, args []string) error {
			var jobs []*controller.JobView
			if err := call("GET", fmt.Sprintf("/api/v1/jobs?limit=%d", limit), nil, &jobs); err != nil {
				return err
			}
			if globalFlags.json {
				return printJSON(jobs)
			}
			t := newTable()
			fmt.Fprintln(t, "ID\tSTATE\tDONE\tRUNNING\tQUEUED\tFAILED\tCPUS\tGPUS\tSUBMITTED\tNAME")
			for _, j := range jobs {
				c := j.Counts
				fmt.Fprintf(t, "%s\t%s\t%d/%d\t%d\t%d\t%d\t%.4g\t%d\t%s\t%s\n", j.ID, j.State,
					c.Succeeded, j.TaskCount, c.Running+c.Assigned, c.Queued, c.Failed,
					j.Spec.CPUs, j.Spec.GPUs, j.CreatedAt.Local().Format("01-02 15:04:05"), j.Name)
			}
			return t.Flush()
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 30, "how many recent jobs to show")

	var state string
	show := &cobra.Command{
		Use:   "show <job-id>",
		Short: "Show a job and its tasks",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var j controller.JobView
			if err := call("GET", "/api/v1/jobs/"+url.PathEscape(args[0]), nil, &j); err != nil {
				return err
			}
			var tasks []*controller.TaskView
			q := "/api/v1/jobs/" + url.PathEscape(j.ID) + "/tasks?limit=1000"
			if state != "" {
				q += "&state=" + url.QueryEscape(state)
			}
			if err := call("GET", q, nil, &tasks); err != nil {
				return err
			}
			if globalFlags.json {
				return printJSON(map[string]any{"job": j, "tasks": tasks})
			}
			s := j.Spec
			fmt.Printf("Job %s (%s): %s\n", j.ID, j.State, j.Name)
			if s.Image != "" {
				fmt.Printf("  image:     %s\n", s.Image)
			}
			fmt.Printf("  command:   %s\n", s.Command)
			fmt.Printf("  resources: %.4g CPU, %s memory, %d GPU per task\n", s.CPUs, humanBytesOr(s.MemoryBytes, "unreserved"), s.GPUs)
			if s.Array != "" {
				fmt.Printf("  array:     %s\n", s.Array)
			}
			fmt.Printf("  retries:   %d, timeout: %s\n\n", s.Retries, orNone(s.TimeoutSec))
			t := newTable()
			fmt.Fprintln(t, "TASK\tSTATE\tATTEMPTS\tNODE\tEXIT\tDURATION\tDETAIL")
			for _, tk := range tasks {
				exit := "-"
				if tk.ExitCode != nil {
					exit = fmt.Sprint(*tk.ExitCode)
				}
				detail := tk.Error
				if tk.PendingReason != "" {
					detail = tk.PendingReason
				}
				fmt.Fprintf(t, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n", tk.ID, tk.State, tk.Attempts, tk.NodeID, exit,
					duration(tk.StartedAt, tk.FinishedAt), detail)
			}
			if len(tasks) == 1000 {
				fmt.Fprintln(t, "...\t(first 1000 tasks shown; filter with --state)")
			}
			return t.Flush()
		},
	}
	show.Flags().StringVar(&state, "state", "", "only tasks in this state (queued, running, succeeded, failed, canceled)")

	rm := &cobra.Command{
		Use:   "rm <job-id>...",
		Short: "Delete finished jobs with their logs",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, id := range args {
				if err := call("DELETE", "/api/v1/jobs/"+url.PathEscape(id), nil, nil); err != nil {
					return fmt.Errorf("%s: %w", id, err)
				}
				fmt.Printf("%s: deleted\n", id)
			}
			return nil
		},
	}
	cmd.AddCommand(show, rm)
	return cmd
}

func humanBytesOr(b uint64, zero string) string {
	if b == 0 {
		return zero
	}
	return humanBytes(b)
}

func orNone(sec int64) string {
	if sec == 0 {
		return "none"
	}
	return (time.Duration(sec) * time.Second).String()
}
