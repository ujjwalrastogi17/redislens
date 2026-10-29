package main

import (
	"fmt"
	"io"
	"os"

	"github.com/ujjwalrastogi17/redislens/internal/report"
)

// openInputs opens every path before any parsing starts, so a typo in the
// last shard fails in milliseconds rather than after the others are read.
// "-" means stdin and may appear once. closeAll is safe to call on error.
func openInputs(paths []string, stdin io.Reader) (inputs []report.Input, closeAll func(), err error) {
	var files []*os.File
	closeAll = func() {
		for _, f := range files {
			f.Close()
		}
	}
	defer func() {
		if err != nil {
			closeAll()
		}
	}()

	var infos []os.FileInfo
	sawStdin := false
	for _, path := range paths {
		if path == "-" {
			if sawStdin {
				return nil, closeAll, usageError{`"-" (stdin) can only be given once`}
			}
			sawStdin = true
			inputs = append(inputs, report.Input{R: stdin, File: "stdin"})
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, closeAll, err
		}
		files = append(files, f)
		info, err := f.Stat()
		if err != nil {
			return nil, closeAll, err
		}
		if !info.Mode().IsRegular() {
			return nil, closeAll, fmt.Errorf("%q is not a regular file", path)
		}
		// os.SameFile also catches the same file under two names (./a.rdb, a.rdb, links).
		for i, prev := range infos {
			if os.SameFile(prev, info) {
				return nil, closeAll, usageError{fmt.Sprintf("%q and %q are the same file", files[i].Name(), path)}
			}
		}
		infos = append(infos, info)
		inputs = append(inputs, report.Input{R: f, File: path, SizeBytes: info.Size()})
	}
	return inputs, closeAll, nil
}
