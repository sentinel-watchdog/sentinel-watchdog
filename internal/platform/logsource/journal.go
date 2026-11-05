package logsource

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strconv"
	"time"
)

// record is one journal entry, or a whole file's tail.
type record struct {
	time time.Time
	text string
}

// parseJournal reads journalctl's JSON lines, oldest first. cut: the first
// line is a fragment of a record older bytes were dropped from. entries counts
// the entries read, which prove the journal readable, also those with no
// MESSAGE, which have nothing to show; skipped counts the lines that could not
// be read.
func parseJournal(stdout []byte, cut bool) (records []record, entries, skipped int) {
	if cut {
		_, stdout, _ = bytes.Cut(stdout, []byte{'\n'})
	}
	for line := range bytes.Lines(stdout) {
		if line[len(line)-1] != '\n' || len(line) > maxRecord {
			skipped++
			continue
		}
		if line = line[:len(line)-1]; len(line) == 0 {
			continue
		}
		rec, found, ok := parseRecord(line)
		switch {
		case !ok:
			skipped++
		case found:
			entries++
			records = append(records, rec)
		default:
			entries++
		}
	}
	return records, entries, skipped
}

func parseRecord(line []byte) (rec record, found, ok bool) {
	var y struct {
		Message  jsontext.Value `json:"MESSAGE"`
		Realtime string         `json:"__REALTIME_TIMESTAMP"`
	}
	if json.Unmarshal(line, &y) != nil {
		return record{}, false, false
	}
	if len(y.Message) == 0 {
		return record{}, false, true
	}
	text, ok := parseMessage(y.Message)
	if !ok {
		return record{}, false, false
	}
	if usec, err := strconv.ParseInt(y.Realtime, 10, 64); err == nil {
		rec.time = time.UnixMicro(usec).UTC()
	}
	rec.text = text
	return rec, true, true
}

// parseMessage reads a journal field as journalctl encodes it: a string; null
// over 4096 bytes (no --all); an array of bytes for what is not valid UTF-8;
// an array of those for a repeated field, of which the first is used.
func parseMessage(v jsontext.Value) (string, bool) {
	switch v.Kind() {
	case '"', 'n':
		return parseSingle(v)
	case '[':
		var items []jsontext.Value
		if json.Unmarshal(v, &items) != nil || len(items) == 0 {
			return "", false
		}
		switch items[0].Kind() {
		case '0':
			return parseBytes(v)
		case '[':
			return parseBytes(items[0])
		}
		return parseSingle(items[0])
	}
	return "", false
}

func parseSingle(v jsontext.Value) (string, bool) {
	switch v.Kind() {
	case 'n':
		return tooLong, true
	case '"':
		var s string
		if json.Unmarshal(v, &s) != nil {
			return "", false
		}
		return s, true
	}
	return "", false
}

func parseBytes(v jsontext.Value) (string, bool) {
	var nums []int
	if json.Unmarshal(v, &nums) != nil {
		return "", false
	}
	out := make([]byte, len(nums))
	for i, n := range nums {
		if n < 0 || n > 255 {
			return "", false
		}
		out[i] = byte(n)
	}
	return string(out), true
}
