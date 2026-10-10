package api

import (
	"bufio"
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
)

// ErrUnsupportedVersion reports a message of another protocol version.
var ErrUnsupportedVersion = errors.New("api: unsupported protocol version")

// ReadRequest reads and checks one request line. The line must end with
// "\n" within MaxRequestBytes and hold exactly one JSON object with known
// members only, no duplicate member, valid UTF-8, version Version (else
// ErrUnsupportedVersion), a valid command name and arguments that are an
// object or absent. Nothing after the newline is read on purpose.
func ReadRequest(r io.Reader) (Request, error) {
	line, err := readLine(r, MaxRequestBytes)
	if err != nil {
		return Request{}, err
	}
	var req Request
	if err := json.Unmarshal(line, &req, json.RejectUnknownMembers(true)); err != nil {
		return Request{}, fmt.Errorf("api: request: %w", err)
	}
	if req.Version != Version {
		return Request{}, fmt.Errorf("%w %d (this end speaks %d)", ErrUnsupportedVersion, req.Version, Version)
	}
	if !ValidCommandName(req.Command) {
		return Request{}, fmt.Errorf("api: invalid command name %q", req.Command)
	}
	if len(req.Args) > 0 && req.Args.Kind() != '{' {
		return Request{}, errors.New("api: args must be a JSON object")
	}
	return req, nil
}

// ReadResponse reads and checks one response line (MaxResponseBytes): the
// right version and exactly one of result and error.
func ReadResponse(r io.Reader) (Response, error) {
	line, err := readLine(r, MaxResponseBytes)
	if err != nil {
		return Response{}, err
	}
	var resp Response
	if err := json.Unmarshal(line, &resp, json.RejectUnknownMembers(true)); err != nil {
		return Response{}, fmt.Errorf("api: response: %w", err)
	}
	if resp.Version != Version {
		return Response{}, fmt.Errorf("%w %d (this end speaks %d)", ErrUnsupportedVersion, resp.Version, Version)
	}
	if (resp.Error == nil) == (len(resp.Result) == 0) {
		return Response{}, errors.New("api: a response holds a result or an error")
	}
	return resp, nil
}

// WriteRequest writes req as one line.
func WriteRequest(w io.Writer, req Request) error {
	return writeLine(w, req, MaxRequestBytes)
}

// WriteResponse writes resp as one line; a response over MaxResponseBytes
// is not written.
func WriteResponse(w io.Writer, resp Response) error {
	return writeLine(w, resp, MaxResponseBytes)
}

// Result returns a response carrying v as its result.
func Result(v any) (Response, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return Response{}, fmt.Errorf("api: encode result: %w", err)
	}
	return Response{Version: Version, Result: jsontext.Value(data)}, nil
}

// Fail returns an error response.
func Fail(code Code, message string) Response {
	return Response{Version: Version, Error: &Error{Code: code, Message: message}}
}

// readLine reads up to and including the first "\n", at most max bytes,
// and returns the line without it.
func readLine(r io.Reader, max int) ([]byte, error) {
	br := bufio.NewReader(io.LimitReader(r, int64(max)))
	line, err := br.ReadBytes('\n')
	switch {
	case err == nil:
		return line[:len(line)-1], nil
	case errors.Is(err, io.EOF) && len(line) >= max:
		return nil, fmt.Errorf("api: message exceeds %d bytes", max)
	case errors.Is(err, io.EOF):
		return nil, errors.New("api: message not ended by a newline")
	default:
		return nil, fmt.Errorf("api: read: %w", err)
	}
}

func writeLine(w io.Writer, v any, max int) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("api: encode: %w", err)
	}
	if bytes.IndexByte(data, '\n') >= 0 { // json escapes newlines in strings
		return errors.New("api: encoded message holds a newline")
	}
	if len(data)+1 > max {
		return fmt.Errorf("api: message of %d bytes exceeds %d", len(data)+1, max)
	}
	_, err = w.Write(append(data, '\n'))
	return err
}
