package logsource

import (
	"encoding/json/v2"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

// shape turns records into the answer. Redaction comes first and sees whole
// records: a secret holding a newline, or lying across a later cut, is masked
// whole. Then the newest n lines, valid UTF-8, each within maxLine, and the
// newest of them within maxResponse of encoded JSON: control characters take
// six bytes each once encoded, so bytes of text say little about the response.
// Truncated covers only these byte bounds: older lines beyond n were not asked
// for, and what is built follows the answer, not the input: a file's lines are
// the program's to choose, and a megabyte of newlines is a million of them.
func shape(records []record, n int, redact func(string) string) Result {
	lines := newestLines(records, n, redact)
	out := Result{Lines: []Line{}}
	size, first := answerOverhead, len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		text, cut := boundLine(lines[i].Text)
		if size += encodedLen(lines[i].Time, text, lines[i].RunID); size > maxResponse {
			out.Truncated = true
			break
		}
		out.Truncated = out.Truncated || cut
		lines[i].Text = text
		first = i
	}
	out.Lines = append(out.Lines, lines[first:]...)
	return out
}

// newestLines returns the last n lines of records, oldest first. It walks back
// from the end, redacting a record only if some of it is needed, and stops at n.
func newestLines(records []record, n int, redact func(string) string) []Line {
	lines := make([]Line, 0, min(n, 64))
	for i := len(records) - 1; i >= 0 && len(lines) < n; i-- {
		text := strings.TrimSuffix(redact(records[i].text), "\n")
		runID := records[i].runID
		if runID != "" && redact(runID) != runID {
			runID = "" // an id that is a secret is no id of ours
		}
		for len(lines) < n {
			at := strings.LastIndexByte(text, '\n')
			lines = append(lines, Line{Time: records[i].time, Text: text[at+1:], RunID: runID})
			if at < 0 {
				break
			}
			text = text[:at]
		}
	}
	slices.Reverse(lines)
	return lines
}

// answerOverhead is the answer around its lines, kept out of the bound for
// them: the array's brackets and api.SupervisorLogsResult's other fields, a
// name of at most 63 bytes included (about 130 bytes; review 3c-5b R1).
const answerOverhead = 256

// encodedLen is the size of a line in the answer, as api.LogLine encodes it,
// plus the comma between lines: measured, not estimated, since everything in it
// is counted against the bound (D-099, D-100). The bound is on the wire type on
// purpose: Line and api.LogLine change together.
func encodedLen(t time.Time, text, runID string) int {
	b, err := json.Marshal(api.LogLine{Time: t, Text: text, RunID: runID})
	if err != nil {
		return maxResponse + 1 // text is valid UTF-8; were it not, the line is left out
	}
	return len(b) + 1
}

// boundLine returns s as valid UTF-8 of at most maxLine bytes, cut on a rune
// boundary, and whether it was cut.
func boundLine(s string) (string, bool) {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= maxLine {
		return s, false
	}
	end := maxLine
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end], true
}

// shapeFile shapes the tail of a file. cut: its first line is a fragment. It
// is dropped after redaction, which can then still mask a secret that began
// on it, and lines may be missing only if fewer than n remain.
func shapeFile(data []byte, cut bool, n int, redact func(string) string) Result {
	text := redact(string(data))
	if cut {
		_, text, _ = strings.Cut(text, "\n")
	}
	if text == "" {
		return Result{Lines: []Line{}, Truncated: cut}
	}
	res := shape([]record{{text: text}}, n, func(s string) string { return s })
	res.Truncated = res.Truncated || (cut && len(res.Lines) < n)
	return res
}
