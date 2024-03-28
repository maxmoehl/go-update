package main

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// ignoreFile reads exclude and include patterns from a file. If the path does
// not exist, no patterns are returned.
func ignoreFile(path string) (exclude, include []string, err error) {
	r, err := os.Open(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("unable to open ignore file")
	} else if errors.Is(err, os.ErrNotExist) {
		// no ignore file
		return nil, nil, nil
	}

	s := bufio.NewScanner(r)
	for s.Scan() {
		l := strings.TrimSpace(s.Text())
		if len(l) == 0 || l[0] == '#' {
			continue
		}

		if l[0] == '!' {
			include = append(include, l)
		} else {
			exclude = append(exclude, l)
		}
	}

	if s.Err() != nil {
		return nil, nil, fmt.Errorf("read ignore file: %w", err)
	}

	return exclude, include, nil
}

// ignore checks whether the string p should be included. If p matches a pattern
// from the exclude list, match will return false unless it also matches a
// pattern from the include list.
func ignore(exclude, include []string, p string) bool {
	matchesExclude := false
	for _, e := range exclude {
		m, err := filepath.Match(e, p)
		if err != nil {
			slog.Warn("failed to match", "error", err)
			continue
		}

		if m {
			matchesExclude = true
			break
		}
	}

	if !matchesExclude {
		return false
	}

	for _, i := range include {
		m, err := filepath.Match(i, p)
		if err != nil {
			slog.Warn("failed to match", "error", err)
			continue
		}

		if m {
			return false
		}
	}

	return true
}
