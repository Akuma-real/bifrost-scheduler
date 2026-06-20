package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

type rotatingFile struct {
	mu         sync.Mutex
	path       string
	maxBytes   int64
	maxBackups int
	file       *os.File
	size       int64
}

func newRotatingFile(path string, maxBytes int64, maxBackups int) (*rotatingFile, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("log max size must be positive")
	}
	if maxBackups < 0 {
		maxBackups = 0
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat log file: %w", err)
	}
	return &rotatingFile{
		path:       path,
		maxBytes:   maxBytes,
		maxBackups: maxBackups,
		file:       file,
		size:       info.Size(),
	}, nil
}

func (f *rotatingFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.file == nil {
		return 0, fmt.Errorf("log file is closed")
	}
	if f.size > 0 && f.size+int64(len(p)) > f.maxBytes {
		if err := f.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := f.file.Write(p)
	f.size += int64(n)
	return n, err
}

func (f *rotatingFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.file == nil {
		return nil
	}
	err := f.file.Close()
	f.file = nil
	return err
}

func (f *rotatingFile) rotate() error {
	if err := f.file.Close(); err != nil {
		return fmt.Errorf("close log before rotation: %w", err)
	}
	f.file = nil

	if f.maxBackups == 0 {
		if err := os.Remove(f.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove old log: %w", err)
		}
	} else {
		oldest := fmt.Sprintf("%s.%d", f.path, f.maxBackups)
		if err := os.Remove(oldest); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove oldest log backup: %w", err)
		}
		for i := f.maxBackups - 1; i >= 1; i-- {
			src := fmt.Sprintf("%s.%d", f.path, i)
			dst := fmt.Sprintf("%s.%d", f.path, i+1)
			if err := os.Rename(src, dst); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("rotate log backup %s: %w", src, err)
			}
		}
		if err := os.Rename(f.path, f.path+".1"); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("rotate current log: %w", err)
		}
	}

	file, err := os.OpenFile(f.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open new log file: %w", err)
	}
	f.file = file
	f.size = 0
	return nil
}

func parseByteSize(value string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, fmt.Errorf("byte size is empty")
	}

	upper := strings.ToUpper(trimmed)
	multipliers := []struct {
		suffix string
		value  int64
	}{
		{suffix: "GB", value: 1024 * 1024 * 1024},
		{suffix: "G", value: 1024 * 1024 * 1024},
		{suffix: "MB", value: 1024 * 1024},
		{suffix: "M", value: 1024 * 1024},
		{suffix: "KB", value: 1024},
		{suffix: "K", value: 1024},
		{suffix: "B", value: 1},
	}
	for _, multiplier := range multipliers {
		if strings.HasSuffix(upper, multiplier.suffix) {
			number := strings.TrimSpace(trimmed[:len(trimmed)-len(multiplier.suffix)])
			parsed, err := parsePositiveInt64(number)
			if err != nil {
				return 0, err
			}
			return parsed * multiplier.value, nil
		}
	}
	return parsePositiveInt64(trimmed)
}

func parsePositiveInt(value string) (int, error) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, err
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("value must be positive")
	}
	return parsed, nil
}

func parsePositiveInt64(value string) (int64, error) {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, err
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("value must be positive")
	}
	return parsed, nil
}
