package executor

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"strconv"
)

// maxStatmBytes bounds /proc/<pid>/statm: seven decimal numbers.
const maxStatmBytes = 512

// parseStatm returns the resident pages of a /proc/<pid>/statm line, its
// second field (proc_pid_statm(5)).
func parseStatm(b []byte) (uint64, error) {
	fields := bytes.Fields(b)
	if len(fields) < 2 {
		return 0, errors.New("statm: too few fields")
	}
	pages, err := strconv.ParseUint(string(fields[1]), 10, 64)
	if err != nil {
		return 0, errors.New("statm: invalid resident size")
	}
	return pages, nil
}

// leaderRSS returns the resident memory in bytes of g's leader, read from
// dir, its /proc/<pid> directory. The leader's stat must match g, else
// the process is ErrGone: its pid names another process by now. Both
// files are read through dir, so when dir is a descriptor opened on the
// leader the second read cannot reach another process either (the
// kernel never resolves an open /proc/<pid> directory to a process that
// reused the pid). The two reads are of one process, not of one instant.
func leaderRSS(dir fs.FS, g Group, pageSize uint64) (uint64, error) {
	b, err := readProc(dir, "stat", maxStatBytes)
	if err != nil {
		return 0, err
	}
	s, err := parseStat(b)
	if err != nil {
		return 0, err
	}
	if s.pid != g.Pgid || s.pgrp != g.Pgid || s.sid != g.Sid || s.start != g.Start || s.gone() {
		return 0, ErrGone
	}
	if b, err = readProc(dir, "statm", maxStatmBytes); err != nil {
		return 0, err
	}
	pages, err := parseStatm(b)
	if err != nil {
		return 0, err
	}
	if pageSize == 0 || pages > math.MaxUint64/pageSize {
		return 0, fmt.Errorf("statm: %d pages of %d bytes overflow", pages, pageSize)
	}
	return pages * pageSize, nil
}
