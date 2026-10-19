package supervisor

import "context"

// Checker runs one check of a service. It is owned here, by the loop that
// consumes it; the variation it isolates is the kind of service (HTTP
// now, systemd later) and the fakes of the tests.
//
// Check must return when ctx ends. The loop bounds one that does not: the
// check counts as timed out, the service starts no other check until it
// returns, and Stop waits for it (D-080, design option C).
type Checker interface {
	Check(ctx context.Context) Result
}

// Result is the outcome of one check. The zero Result is a success.
type Result struct {
	// Failure is a fixed code, never error text: a check's errors may quote
	// URLs, headers or bodies, and codes are the only redaction that holds
	// for short or encoded secrets (D-080 (4)).
	Failure Failure
}

// OK reports whether the check succeeded.
func (r Result) OK() bool { return r.Failure == "" }

// Failure is the fixed code of a failed check.
type Failure string

// Failure codes of the loop.
const (
	// FailureCheck is a failure the checker does not classify further.
	FailureCheck Failure = "check_failed"
	// FailureTimeout: the check did not return within the service timeout.
	FailureTimeout Failure = "timeout"
	// FailurePanic: the checker panicked.
	FailurePanic Failure = "panic"
	// FailureInvalid: the checker returned a code outside this list.
	FailureInvalid Failure = "invalid_result"
)

// Failure codes of the HTTP checker (D-082).
const (
	FailureDNS             Failure = "dns_failed"         // the URL's host did not resolve
	FailureConnect         Failure = "connect_failed"     // no connection (refused, unreachable)
	FailureTLS             Failure = "tls_failed"         // handshake or certificate verification
	FailureRequest         Failure = "request_failed"     // any other transport failure
	FailureStatus          Failure = "status_mismatch"    // a status code outside success_status_codes
	FailureBody            Failure = "body_mismatch"      // body_contains not found
	FailureBodyRead        Failure = "body_read_failed"   // the body could not be read
	FailureRedirectRefused Failure = "redirect_refused"   // a redirect to another origin
	FailureRedirects       Failure = "too_many_redirects" // more than maxRedirects hops
)

// known reports whether f is one of the codes above (or empty).
func (f Failure) known() bool {
	switch f {
	case "", FailureCheck, FailureTimeout, FailurePanic, FailureInvalid,
		FailureDNS, FailureConnect, FailureTLS, FailureRequest, FailureStatus,
		FailureBody, FailureBodyRead, FailureRedirectRefused, FailureRedirects:
		return true
	}
	return false
}

// normalize returns r with an unknown code replaced by FailureInvalid, so
// free text a checker put in Failure never reaches state, events or status.
func normalize(r Result) Result {
	if !r.Failure.known() {
		return Result{Failure: FailureInvalid}
	}
	return r
}
