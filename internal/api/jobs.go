package api

import (
	"net/http"
	"strconv"

	"github.com/NTFespolion307/QuorvexFusion/internal/controller"
	"github.com/NTFespolion307/QuorvexFusion/internal/store"
)

func intParam(r *http.Request, name string, def, max int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || v < 0 {
		return def
	}
	return min(v, max)
}

func (s *Server) submitJob(w http.ResponseWriter, r *http.Request) {
	var spec controller.JobSpec
	if err := readJSON(r, &spec); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	job, err := s.c.SubmitJob(&spec)
	if err != nil {
		writeErr(w, err)
		return
	}
	v, err := s.c.GetJobView(job.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.c.ListJobViews(intParam(r, "limit", 50, 1000))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	v, err := s.c.GetJobView(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) listJobTasks(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.c.Store().GetJob(id); err != nil {
		writeErr(w, err)
		return
	}
	tasks, err := s.c.ListTaskViews(id, store.TaskState(r.URL.Query().Get("state")),
		intParam(r, "limit", 1000, 10000), intParam(r, "offset", 0, 1<<30))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tasks)
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	if err := s.c.CancelJob(r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteJob(w http.ResponseWriter, r *http.Request) {
	if err := s.c.DeleteJob(r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	v, err := s.c.GetTaskView(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) cancelTask(w http.ResponseWriter, r *http.Request) {
	if err := s.c.CancelTask(r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// taskLogs returns raw log bytes from ?offset= (default 0), at most 1 MiB
// per request. Response headers tell the client how to continue:
//
//	X-Log-Size     total bytes in the stream so far
//	X-Log-Attempt  which attempt's log this is
//	X-Task-State   the task's state (stop following once it is terminal)
//
// Query: stream=combined|stdout|stderr, attempt=N (default latest).
func (s *Server) taskLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	stream := controller.LogStream(q.Get("stream"))
	switch stream {
	case "":
		stream = controller.LogCombined
	case controller.LogCombined, controller.LogStdout, controller.LogStderr:
	default:
		writeError(w, http.StatusBadRequest, "stream must be combined, stdout or stderr")
		return
	}
	offset, _ := strconv.ParseInt(q.Get("offset"), 10, 64)
	attempt, _ := strconv.Atoi(q.Get("attempt"))
	id := r.PathValue("id")

	// Read the state first: if it is terminal, the log read below is
	// guaranteed to include everything the task wrote.
	t, err := s.c.Store().GetTask(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	data, size, used, err := s.c.ReadLog(id, attempt, stream, max(offset, 0), 1<<20)
	if err != nil {
		writeErr(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Log-Size", strconv.FormatInt(size, 10))
	h.Set("X-Log-Attempt", strconv.Itoa(used))
	h.Set("X-Task-State", string(t.State))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
