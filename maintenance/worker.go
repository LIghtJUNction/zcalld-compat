package maintenance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"
)

const workerFrameBytes = 16 << 10

type weeklyParams struct {
	Account   string        `json:"account"`
	Trigger   WeeklyTrigger `json:"trigger"`
	TimeoutMS int64         `json:"timeout_ms"`
}

type workerRequest struct {
	ID      uint64          `json:"id"`
	JSONRPC string          `json:"jsonrpc,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func strictWorkerJSON(data []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(value) != nil {
		return ErrInvalid
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return ErrInvalid
	}
	return nil
}

// ServeWorker processes exactly one bounded request. The input is private
// parent-child stdio, not an HTTP management endpoint. It keeps reading solely
// for the native compat client's abort frame or EOF while the operation runs.
// The callback must carry ctx through its host call and credential drain.
func ServeWorker(ctx context.Context, input io.ReadCloser, output io.Writer, run func(context.Context, string, WeeklyTrigger) (ActionResult, error)) error {
	defer input.Close()
	scan := bufio.NewScanner(input)
	scan.Buffer(make([]byte, 4096), workerFrameBytes)
	if !scan.Scan() {
		if scan.Err() != nil {
			return errors.New("invalid maintenance worker input")
		}
		return nil
	}
	var request workerRequest
	var params weeklyParams
	if strictWorkerJSON(scan.Bytes(), &request) != nil || request.ID == 0 || request.Method != "weekly" ||
		(request.JSONRPC != "" && request.JSONRPC != "2.0") || strictWorkerJSON(request.Params, &params) != nil ||
		!accountPattern.MatchString(params.Account) || !validTrigger(params.Trigger) || params.TimeoutMS < 1 || params.TimeoutMS > 60000 || run == nil {
		return errors.New("invalid maintenance worker request")
	}
	operation, cancel := context.WithTimeout(ctx, time.Duration(params.TimeoutMS)*time.Millisecond)
	defer cancel()
	go func() {
		defer cancel() // EOF, oversized control or invalid traffic all stop work.
		for scan.Scan() {
			var control struct {
				Method string `json:"method"`
				Params struct {
					ID uint64 `json:"id"`
				} `json:"params"`
			}
			if strictWorkerJSON(scan.Bytes(), &control) != nil {
				return
			}
			if (control.Method == "abort" || control.Method == "cancel") && control.Params.ID == request.ID {
				return
			}
			// This process owns one request; there is no other operation whose
			// cancellation or second call it can legitimately receive.
			return
		}
	}()
	result, err := run(operation, params.Account, params.Trigger)
	if result.Status != "completed" && result.Status != "not_sent" && result.Status != "unknown" {
		result = ActionResult{Status: "unknown", Code: "maintenance_unavailable"}
	}
	if err != nil && result.Code == "" {
		result.Code = "maintenance_unavailable"
	}
	if len(result.Code) > 64 {
		result = ActionResult{Status: "unknown", Code: "maintenance_unavailable"}
	}
	// A caller cannot mistake an arbitrary successful helper action for the
	// exact positively acknowledged reset that authorizes a request retry.
	response := struct {
		ID     uint64       `json:"id"`
		Result ActionResult `json:"result"`
	}{request.ID, result}
	writeErr := json.NewEncoder(output).Encode(response)
	_ = input.Close()
	// An inherited blocking stdin is not guaranteed to unblock a concurrent
	// Read when Close is called on every platform. This is a one-request
	// process: the completed operation is drained, and main exits immediately.
	// Do not wait for a passive control reader after producing the result.
	return writeErr
}
