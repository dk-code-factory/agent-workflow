package operational

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var ErrUnavailable = errors.New("execution prerequisites unavailable")

type Config struct {
	MaxQueued     int     `yaml:"max-queued"`
	MaxRuns       int     `yaml:"max-active-runs"`
	RunTimeout    string  `yaml:"run-timeout"`
	MinFreeBytes  *uint64 `yaml:"min-free-bytes"`
	MinFreeInodes *uint64 `yaml:"min-free-inodes"`
	TempDirectory string  `yaml:"temporary-directory"`
}

func (c Config) Validate() error {
	if c.MaxQueued < 0 || c.MaxRuns < 0 {
		return fmt.Errorf("controller limits cannot be negative")
	}
	if c.RunTimeout != "" {
		duration, err := time.ParseDuration(c.RunTimeout)
		if err != nil || duration <= 0 {
			return fmt.Errorf("controller run-timeout must be positive")
		}
	}
	return nil
}
func (c Config) Timeout() time.Duration {
	if c.RunTimeout == "" {
		return 30 * time.Minute
	}
	duration, _ := time.ParseDuration(c.RunTimeout)
	return duration
}

func CheckPrivateState(dir string) error {
	for _, path := range []string{dir, filepath.Join(dir, "state.db")} {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("state directory/database permissions must exclude group and other access")
		}
	}
	return nil
}

type Filesystem struct {
	Path       string `json:"path"`
	FreeBytes  uint64 `json:"free_bytes"`
	FreeInodes uint64 `json:"free_inodes"`
}

func (c Config) Thresholds() (uint64, uint64) {
	bytes, inodes := uint64(64<<20), uint64(128)
	if c.MinFreeBytes != nil {
		bytes = *c.MinFreeBytes
	}
	if c.MinFreeInodes != nil {
		inodes = *c.MinFreeInodes
	}
	return bytes, inodes
}
func (c Config) Check(path string) (Filesystem, error) {
	state, err := StatFilesystem(path)
	if err != nil {
		return state, err
	}
	bytes, inodes := c.Thresholds()
	if state.FreeBytes < bytes {
		return state, fmt.Errorf("filesystem free bytes are below admission threshold")
	}
	if state.FreeInodes < inodes {
		return state, fmt.Errorf("filesystem free inodes are below admission threshold")
	}
	return state, nil
}
func (c Config) TemporaryDirectory() string {
	if c.TempDirectory != "" {
		return c.TempDirectory
	}
	return os.TempDir()
}

type RotatingLog struct {
	mu       sync.Mutex
	Path     string
	MaxBytes int64
	Backups  int
	file     *os.File
	size     int64
}

func (l *RotatingLog) Write(data []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.MaxBytes <= 0 {
		l.MaxBytes = 10 << 20
	}
	if l.Backups <= 0 {
		l.Backups = 3
	}
	if l.file == nil {
		if err := os.MkdirAll(filepath.Dir(l.Path), 0700); err != nil {
			return 0, err
		}
		file, err := os.OpenFile(l.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return 0, err
		}
		l.file = file
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			l.file = nil
			return 0, err
		}
		l.size = info.Size()
	}
	if l.size > 0 && l.size+int64(len(data)) > l.MaxBytes {
		if err := l.file.Close(); err != nil {
			return 0, err
		}
		l.file = nil
		for i := l.Backups; i >= 1; i-- {
			target := fmt.Sprintf("%s.%d", l.Path, i)
			source := l.Path
			if i > 1 {
				source = fmt.Sprintf("%s.%d", l.Path, i-1)
			}
			if err := os.Rename(source, target); err != nil && !os.IsNotExist(err) {
				return 0, err
			}
		}
		file, err := os.OpenFile(l.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return 0, err
		}
		l.file = file
		l.size = 0
	}
	n, err := l.file.Write(data)
	l.size += int64(n)
	return n, err
}
func (l *RotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
