// Package env loads environment variables from an adjacent ".env" file, so
// credentials don't have to be exported in the shell before running one of
// this module's commands.
package env

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Load looks for a ".env" file next to the running program (either its
// source directory, when run via "go run .", or next to the compiled
// binary) and, failing that, in the current directory. If found, each
// KEY=VALUE line is applied via os.Setenv, but only for keys that aren't
// already set in the real environment - actual environment variables always
// win over the .env file.
//
// It is not an error for the .env file to be missing; this is purely a
// convenience so credentials don't have to be exported in the shell.
func Load() error {
	for _, dir := range searchDirs() {
		path := filepath.Join(dir, ".env")
		if err := applyFile(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("reading %s: %w", path, err)
		}
		return nil
	}
	return nil
}

// searchDirs returns the candidate directories to look for a ".env" file
// in, in priority order: the directory of the compiled binary, then the
// current working directory.
func searchDirs() []string {
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		dirs = append(dirs, filepath.Dir(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}
	return dirs
}

// applyFile parses a single .env file and applies its variables to the
// process environment (without overriding anything already set).
func applyFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")

		idx := strings.Index(line, "=")
		if idx < 0 {
			return fmt.Errorf("line %d: expected KEY=VALUE, got %q", lineNum, line)
		}
		key := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])
		if key == "" {
			return fmt.Errorf("line %d: empty key", lineNum)
		}
		value = unquoteValue(value)

		if _, already := os.LookupEnv(key); !already {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf("line %d: setting %s: %w", lineNum, key, err)
			}
		}
	}
	return scanner.Err()
}

// unquoteValue strips a single layer of matching single or double quotes
// from a .env value, if present, so `KEY="some value"` and `KEY='some
// value'` behave the same as `KEY=some value`.
func unquoteValue(value string) string {
	if len(value) >= 2 {
		first, last := value[0], value[len(value)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}
