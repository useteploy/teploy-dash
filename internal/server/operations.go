package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/operation"
)

func executeOperation(ctx context.Context, command operation.Command, emit func(operation.Stream, string)) (int, error) {
	result, err := cli.RunStreamStdin(ctx, command.Stdin, command.Args, command.Timeout, func(event cli.StreamEvent) {
		stream := operation.StreamStdout
		if event.Stream == cli.StreamStderr {
			stream = operation.StreamStderr
		}
		emit(stream, event.Data)
	})
	if result == nil {
		return -1, err
	}
	if err != nil {
		return result.ExitCode, err
	}
	if result.ExitCode != 0 {
		return result.ExitCode, fmt.Errorf("teploy exited with code %d", result.ExitCode)
	}
	return 0, nil
}

func (s *Server) resolveOperationServer(name string) (operation.Server, error) {
	srv, ok := s.lookupServer(name)
	if !ok {
		return operation.Server{}, fmt.Errorf("server not found: %s", name)
	}
	return operation.Server{Name: srv.Name, Host: srv.Host, User: srv.User}, nil
}

func (s *Server) handleOperations(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !s.operationsAvailable(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		limit := 100
		if raw := r.URL.Query().Get("limit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 500 {
				writeError(w, "limit must be between 1 and 500")
				return
			}
			limit = parsed
		}
		status := operation.Status(r.URL.Query().Get("status"))
		if status != "" && !validOperationStatus(status) {
			writeError(w, "invalid operation status")
			return
		}
		writeData(w, s.operations.List(status, r.URL.Query().Get("target"), limit))
	case http.MethodPost:
		var request operation.Request
		// Same strict single-object policy as strictDecode: exactly one JSON
		// value, no unknown fields (A29).
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			writeError(w, "invalid request body: "+err.Error())
			return
		}
		var extra json.RawMessage
		if err := decoder.Decode(&extra); err != io.EOF {
			writeError(w, "request body must contain exactly one JSON object")
			return
		}
		s.enqueueOperation(w, r, request)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleOperation(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !s.operationsAvailable(w) {
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/operations/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeErrorStatus(w, "operation not found", http.StatusNotFound)
		return
	}
	id := parts[0]
	if len(parts) == 1 && r.Method == http.MethodGet {
		op, err := s.operations.Get(id)
		if err != nil {
			writeOperationError(w, err)
			return
		}
		writeData(w, op)
		return
	}
	if len(parts) != 2 {
		writeErrorStatus(w, "operation route not found", http.StatusNotFound)
		return
	}
	switch parts[1] {
	case "events":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleOperationEvents(w, r, id)
	case "cancel":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		op, err := s.operations.Cancel(id)
		if err != nil {
			writeOperationError(w, err)
			return
		}
		writeData(w, op)
	case "retry":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// The retry is re-authorized by the caller, so it is attributed to
		// them; RetryOf on the new record preserves the lineage.
		op, err := s.operations.Retry(id, actorFromRequest(r))
		if err != nil {
			writeOperationError(w, err)
			return
		}
		writeAcceptedOperation(w, op, false)
	default:
		writeErrorStatus(w, "operation route not found", http.StatusNotFound)
	}
}

func (s *Server) enqueueOperation(w http.ResponseWriter, r *http.Request, request operation.Request) {
	noStore(w)
	if !s.operationsAvailable(w) {
		return
	}
	op, replayed, err := s.operations.Enqueue(request, r.Header.Get("Idempotency-Key"), actorFromRequest(r))
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeAcceptedOperation(w, op, replayed)
}

func writeAcceptedOperation(w http.ResponseWriter, op *operation.Operation, replayed bool) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Location", "/api/operations/"+op.ID)
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]interface{}{"data": op})
}

func (s *Server) operationsAvailable(w http.ResponseWriter) bool {
	if s.operations != nil {
		return true
	}
	message := "operation service unavailable"
	if s.operationInitErr != nil {
		message += ": " + s.operationInitErr.Error()
	}
	writeErrorStatus(w, message, http.StatusServiceUnavailable)
	return false
}

func writeOperationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, operation.ErrNotFound):
		writeErrorStatus(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, operation.ErrAdmissionBudget), errors.Is(err, operation.ErrGlobalAdmissionBudget):
		writeErrorStatus(w, err.Error(), http.StatusTooManyRequests)
	case errors.Is(err, operation.ErrShuttingDown):
		// F022: admission closed by shutdown — a clean, retryable refusal
		// for requests that outlived the HTTP drain.
		writeErrorStatus(w, err.Error(), http.StatusServiceUnavailable)
	case errors.Is(err, operation.ErrIdempotencyConflict), errors.Is(err, operation.ErrNotCancelable), errors.Is(err, operation.ErrNotRetryable):
		writeErrorStatus(w, err.Error(), http.StatusConflict)
	default:
		writeError(w, err.Error())
	}
}

func validOperationStatus(status operation.Status) bool {
	switch status {
	case operation.StatusQueued, operation.StatusRunning, operation.StatusCancelRequested,
		operation.StatusSucceeded, operation.StatusFailed, operation.StatusCanceled, operation.StatusInterrupted:
		return true
	default:
		return false
	}
}

func (s *Server) handleOperationEvents(w http.ResponseWriter, r *http.Request, id string) {
	lastSequence, err := lastEventSequence(r)
	if err != nil {
		writeError(w, err.Error())
		return
	}
	replay, notifications, terminal, err := s.operations.Subscribe(id, lastSequence)
	if err != nil {
		writeOperationError(w, err)
		return
	}
	if !terminal {
		defer s.operations.Unsubscribe(id, notifications)
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErrorStatus(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// R60: every write carries a deadline (a non-reading client cannot pin
	// the handler), the stream opens with a comment frame, and a heartbeat
	// keeps proxies from dropping a silent connection.
	controller := http.NewResponseController(w)
	writeRaw := func(frame []byte) bool {
		// A deadline is best-effort: test recorders (and exotic handlers)
		// may not support instrumentation — only a WRITE failure aborts.
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := w.Write(frame); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !writeRaw([]byte(": connected\n\n")) {
		return
	}

	writeEvents := func(events []operation.Event) bool {
		for _, event := range events {
			data, err := json.Marshal(event)
			if err != nil {
				return false
			}
			if !writeRaw([]byte(fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", event.Sequence, event.Type, data))) {
				return false
			}
			lastSequence = event.Sequence
		}
		return true
	}
	if !writeEvents(replay) {
		return
	}
	if terminal {
		// A53: an explicit end-of-replay marker terminates the stream for
		// terminal histories — the client no longer has to guess from a
		// refetched snapshot and close early (losing replayed output) or
		// reconnect forever when the retained history is empty.
		writeRaw([]byte("event: replay-complete\ndata: {\"terminal\":true}\n\n"))
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case _, open := <-notifications:
			events, err := s.operations.EventsAfter(id, lastSequence)
			if err != nil || !writeEvents(events) || !open {
				// R59: the notification channel closes when the operation
				// finished and its subscribers were released — the retained
				// replay is complete, so say so instead of dropping the
				// connection without the marker (a reconnecting client
				// could otherwise close on a terminal snapshot before the
				// final output is replayed).
				if err == nil && !open {
					writeRaw([]byte("event: replay-complete\ndata: {\"terminal\":true}\n\n"))
				}
				return
			}
		case <-heartbeat.C:
			if !writeRaw([]byte(": heartbeat\n\n")) {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

func lastEventSequence(r *http.Request) (uint64, error) {
	value := r.Header.Get("Last-Event-ID")
	if value == "" {
		value = r.URL.Query().Get("after")
	}
	if value == "" {
		return 0, nil
	}
	sequence, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid Last-Event-ID")
	}
	return sequence, nil
}
