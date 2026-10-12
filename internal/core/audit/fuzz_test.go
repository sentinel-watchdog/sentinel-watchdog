package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const goldenSeed = `{"seq":1,"time":"2026-10-12T10:00:00Z","actor":{"kind":"socket","uid":0,"gid":0,"pid":42},` +
	`"action":"core.reload","result":"intent","correlation_id":"c1","detail":{"a":"1","b":"2"},` +
	`"prev_hash":"sha256:0000000000000000000000000000000000000000000000000000000000000000",` +
	`"hash":"sha256:6649cf0e5f32164560c05b1adae717010008a5e04327fe388e3485643c5ac58c"}`

// seal turns a JSON object into a line with its hash, so the fuzzer gets
// past the hash check and reaches the decoder.
func seal(body []byte) []byte {
	if len(body) == 0 || body[len(body)-1] != '}' {
		return body
	}
	sum := sha256.Sum256(body)
	line := append([]byte{}, body[:len(body)-1]...)
	line = append(line, hashMember+hashPrefix+hex.EncodeToString(sum[:])+`"}`...)
	return line
}

// FuzzParse: a line accepted by parse holds a valid record, and writing
// that record again gives a line that parses to the same record. Every
// input is tried as is and sealed with its hash.
func FuzzParse(f *testing.F) {
	f.Add([]byte(goldenSeed))
	f.Add([]byte(goldenSeed[:strings.Index(goldenSeed, `,"hash"`)] + "}"))
	f.Add([]byte(`{"seq":1,"seq":2}`))
	f.Add([]byte(`{"seq":1,"time":"2026-10-12T10:00:00Z","actor":{"kind":"signal"},"action":"core.reload","result":"ok","prev_hash":"x","extra":1}`))
	f.Fuzz(func(t *testing.T, line []byte) {
		check(t, line)
		check(t, seal(line))
	})
}

func check(t *testing.T, line []byte) {
	r, err := parse(line)
	if err != nil {
		return
	}
	if r.Seq == 0 || !validHash(r.Hash) || !validHash(r.PrevHash) {
		t.Fatalf("accepted an invalid record: %+v", r)
	}
	again, _, err := encode(r)
	if err != nil {
		return // a record near the size limit may encode longer
	}
	r2, err := parse(again[:len(again)-1])
	if err != nil {
		t.Fatalf("re-encoded record does not parse: %v", err)
	}
	r.Hash, r2.Hash = "", ""
	for _, rec := range []*Record{&r, &r2} { // empty and absent details are the same record
		if len(rec.Detail) == 0 {
			rec.Detail = nil
		}
	}
	if !reflect.DeepEqual(r, r2) {
		t.Fatalf("round trip changed the record:\n%+v\n%+v", r, r2)
	}
}

// FuzzOpen: whatever the active file holds, Open checks it without
// panicking or hanging, and never reports a chain it could not verify as
// intact.
func FuzzOpen(f *testing.F) {
	f.Add([]byte(goldenSeed + "\n"))
	f.Add([]byte(goldenSeed))
	f.Add([]byte("\n\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		dir := filepath.Join(t.TempDir(), "state")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		// A head that knows the golden record, as a daemon that wrote it.
		h := `{"version":1,"last":{"seq":1,"hash":"sha256:6649cf0e5f32164560c05b1adae717010008a5e04327fe388e3485643c5ac58c"},` +
			`"anchor":{"seq":0,"hash":"` + genesisHash + `"}}`
		if err := os.WriteFile(filepath.Join(dir, headFile), []byte(h), fileMode); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, activeFile), data, fileMode); err != nil {
			t.Fatal(err)
		}
		l, err := tryOpen(dir, 0, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close(context.Background())
		if l.Integrity().State == StateOK && string(data) != goldenSeed+"\n" {
			t.Fatalf("accepted %q", data)
		}
	})
}
