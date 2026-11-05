package filecache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	defaultMaxSize         = 1024 * 1024 * 1024 // 1GB
	defaultMaxTTL          = 4 * time.Hour      // 4 hours
	defaultBaseDir         = "filecache"
	defaultCleanupInterval = 5 * time.Minute
	defaultLockKey         = "lock_filecache"
	defaultLogLevel        = logrus.WarnLevel
	defaultDirFileMode     = os.FileMode(0777)
	tempFilePrefix         = "filecachetmp-"
)

var (
	// ErrKeyExisted is returned by Write when the key is already cached.
	ErrKeyExisted = errors.New("key existed")
	// ErrInvalidKey is returned when a key is empty, ".", ".." or contains
	// a path separator or NUL byte.
	ErrInvalidKey = errors.New("invalid key")
	// ErrLocked is returned by Read when the key is locked by a Write or
	// Delete in progress. Callers may retry or treat it as a cache miss.
	ErrLocked = errors.New("has locked")
)

type Config struct {
	BaseDir         string
	TempDir         string
	MaxSize         int64
	MaxTTL          time.Duration
	CleanupInterval time.Duration
	// LogLevel defaults to logrus.WarnLevel when left as zero, so GC
	// failures are reported. Zero is logrus.PanicLevel and cannot be chosen
	// here; to silence logs set fc.Logger.Out to io.Discard after New.
	LogLevel logrus.Level
}

type ILock interface {
	Unlock(ctx context.Context)
}

type ILockFatory interface {
	Lock(ctx context.Context, key string) (ILock, error)
	Has(ctx context.Context, key string) bool
}

type FileCache struct {
	Config
	lockFactory ILockFatory
	quit        chan struct{}
	stopGC      sync.Once
	Logger      *logrus.Logger

	// tempMu guards activeTemps, the temp files of in-flight writes,
	// so Empty never removes a temp file that a Write is still using.
	tempMu      sync.Mutex
	activeTemps map[string]struct{}
}

func ensureDir(dir string) (string, error) {
	if absdir, err := filepath.Abs(dir); err != nil {
		return "", err
	} else {
		dir = absdir
	}
	if err := os.MkdirAll(dir, defaultDirFileMode); err != nil {
		return "", err
	}
	return dir, nil
}

func New(config Config, lockFactory ILockFatory) *FileCache {
	fc := &FileCache{Config: config, lockFactory: lockFactory, quit: make(chan struct{})}
	if len(fc.BaseDir) == 0 {
		fc.BaseDir = defaultBaseDir
	}
	if dir, err := ensureDir(fc.BaseDir); err != nil {
		panic(err)
	} else {
		fc.BaseDir = dir
	}

	if len(fc.TempDir) == 0 {
		fc.TempDir = filepath.Join(fc.BaseDir, ".tmp")
	}
	if dir, err := ensureDir(fc.TempDir); err != nil {
		panic(err)
	} else {
		fc.TempDir = dir
	}
	if fc.MaxSize == 0 {
		fc.MaxSize = defaultMaxSize
	}
	if fc.MaxTTL == 0 {
		fc.MaxTTL = defaultMaxTTL
	}
	if fc.CleanupInterval == 0 {
		fc.CleanupInterval = defaultCleanupInterval
	}
	if fc.LogLevel == 0 {
		fc.LogLevel = defaultLogLevel
	}
	fc.Logger = &logrus.Logger{
		Out:          os.Stderr,
		Formatter:    new(logrus.TextFormatter),
		Hooks:        make(logrus.LevelHooks),
		Level:        fc.LogLevel,
		ExitFunc:     os.Exit,
		ReportCaller: false,
	}
	return fc
}

func checkFileExist(asbFilePath string) error {
	fs, err := os.Stat(asbFilePath)
	if err != nil {
		return err
	}
	if fs.IsDir() {
		return os.ErrNotExist
	}
	return nil
}

func keylock(key string) string {
	return fmt.Sprintf("%s_%s", defaultLockKey, key)
}

func validateKey(key string) error {
	if key == "" || key == "." || key == ".." {
		return ErrInvalidKey
	}
	if filepath.IsAbs(key) || strings.ContainsAny(key, `/\`) || strings.ContainsRune(key, 0) {
		return ErrInvalidKey
	}
	return nil
}

func (f *FileCache) absFilePath(key string) string {
	return filepath.Join(f.BaseDir, key)
}

func (f *FileCache) safeFilePath(key string) (string, error) {
	if err := validateKey(key); err != nil {
		return "", err
	}
	return f.absFilePath(key), nil
}

func (f *FileCache) hasFile(key string) (string, error) {
	absFilePath, err := f.safeFilePath(key)
	if err != nil {
		return "", err
	}
	if err := checkFileExist(absFilePath); err != nil {
		return absFilePath, err
	}
	return absFilePath, nil
}

// Read returns an IO stream of file reader. A missing key returns an error
// matching fs.ErrNotExist; a locked key returns ErrLocked.
func (f *FileCache) Read(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	if f.lockFactory != nil {
		if f.lockFactory.Has(ctx, keylock(key)) {
			return nil, ErrLocked
		}
	}

	absFilePath, err := f.hasFile(key)
	if err != nil {
		return nil, err
	}

	if err := f.touch(key, time.Now()); err != nil {
		return nil, err
	}

	file, err := os.Open(absFilePath)
	if err != nil {
		return nil, err
	}
	return file, nil
}

func (f *FileCache) Has(key string) bool {
	_, err := f.hasFile(key)
	return err == nil
}

// Write writes an file to disk
func (f *FileCache) Write(ctx context.Context, key string, r io.Reader) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if f.lockFactory != nil {
		lock, err := f.lockFactory.Lock(ctx, keylock(key))
		if err != nil {
			return err
		}
		defer lock.Unlock(ctx)
	}

	absFilePath, err := f.hasFile(key)
	if err == nil {
		return ErrKeyExisted
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	tmp, err := f.createTemp()
	if err != nil {
		return err
	}
	defer f.releaseTemp(tmp.Name())
	tmpClosed := false
	defer func() {
		if !tmpClosed {
			tmp.Close()
		}
	}()
	defer os.Remove(tmp.Name())

	if _, err := io.Copy(tmp, r); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	tmpClosed = true

	if err := os.Link(tmp.Name(), absFilePath); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return ErrKeyExisted
		}
		return err
	}
	return nil
}

func (f *FileCache) Delete(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if f.lockFactory != nil {
		lock, err := f.lockFactory.Lock(ctx, keylock(key))
		if err != nil {
			return err
		}
		defer lock.Unlock(ctx)
	}

	absFilePath, err := f.hasFile(key)
	if err != nil {
		return err
	}
	return os.Remove(absFilePath)
}

func (f *FileCache) Empty(ctx context.Context) error {
	if f.lockFactory != nil {
		lock, err := f.lockFactory.Lock(ctx, defaultLockKey)
		if err != nil {
			return err
		}
		defer lock.Unlock(ctx)
	}

	if err := removeCachedFiles(f.BaseDir); err != nil {
		return err
	}
	if err := f.removeTempFiles(); err != nil {
		return err
	}
	if _, err := ensureDir(f.BaseDir); err != nil {
		return err
	}
	if _, err := ensureDir(f.TempDir); err != nil {
		return err
	}
	return nil
}

func (fc *FileCache) touch(key string, ts time.Time) error {
	if ts.IsZero() {
		ts = time.Now()
	}
	absFilePath, err := fc.safeFilePath(key)
	if err != nil {
		return err
	}
	return os.Chtimes(absFilePath, ts, ts)
}

// createTemp creates a temp file for a Write and registers it as in-flight.
func (f *FileCache) createTemp() (*os.File, error) {
	f.tempMu.Lock()
	defer f.tempMu.Unlock()

	tmp, err := os.CreateTemp(f.TempDir, tempFilePrefix)
	if err != nil {
		return nil, err
	}
	if f.activeTemps == nil {
		f.activeTemps = make(map[string]struct{})
	}
	f.activeTemps[filepath.Base(tmp.Name())] = struct{}{}
	return tmp, nil
}

func (f *FileCache) releaseTemp(name string) {
	f.tempMu.Lock()
	defer f.tempMu.Unlock()
	delete(f.activeTemps, filepath.Base(name))
}

// removeCachedFiles removes cache entries only. Keys cannot contain path
// separators, so every entry is a top-level non-directory; subdirectories
// (such as the default TempDir) are left untouched.
func removeCachedFiles(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// removeTempFiles removes leftover cache temp files, skipping those that
// belong to in-flight writes of this FileCache.
func (f *FileCache) removeTempFiles() error {
	f.tempMu.Lock()
	defer f.tempMu.Unlock()

	entries, err := os.ReadDir(f.TempDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), tempFilePrefix) {
			continue
		}
		if _, active := f.activeTemps[entry.Name()]; active {
			continue
		}
		if err := os.RemoveAll(filepath.Join(f.TempDir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func byte2MB(b int64) int64 {
	return b / (1024 * 1024)
}

// Files reads the directory named by dirname and returns
// a list of fs.FileInfo for the directory's contents,
// sorted by modification time. If an error occurs reading the directory,
// Files returns no directory entries along with the error.
func (fc *FileCache) Files() ([]fs.FileInfo, error) {
	f, err := os.Open(fc.BaseDir)
	if err != nil {
		return nil, err
	}
	list, err := f.Readdir(-1)
	f.Close()
	if err != nil {
		return nil, err
	}
	files := list[:0]
	for _, info := range list {
		if !info.IsDir() {
			files = append(files, info)
		}
	}
	list = files
	sort.Slice(list, func(i, j int) bool {
		iModTime := list[i].ModTime()
		jModTime := list[j].ModTime()
		if iModTime.Equal(jModTime) {
			return list[i].Name() < list[j].Name()
		}
		return iModTime.Before(jModTime)
	})
	return list, nil
}

func (fc *FileCache) cleanCachedFileByTTL(ctx context.Context) error {
	files, err := fc.Files()
	if err != nil {
		return err
	}

	log := fc.Logger.WithField("strategy", "TTL")
	var firstErr error
	count := 0
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Since(file.ModTime()) <= fc.MaxTTL {
			continue
		}
		if err := fc.gcDelete(ctx, file.Name()); err != nil {
			log.WithError(err).Warnf("Failed to clean cache file %s", file.Name())
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		count++
		log.Debugf("Cleaned cache file %s", file.Name())
	}
	log.Infof("Cleaned %v files", count)
	return firstErr
}

// gcDelete deletes a cache entry on behalf of GC. An entry that is already
// gone (deleted concurrently) counts as successfully cleaned.
func (fc *FileCache) gcDelete(ctx context.Context, key string) error {
	if err := fc.Delete(ctx, key); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Size returns the total size of cached entries. Temp files of in-flight
// writes are not counted, matching the entries GC is able to evict.
func (fc *FileCache) Size() (int64, error) {
	files, err := fc.Files()
	if err != nil {
		return 0, err
	}
	return totalSize(files), nil
}

func totalSize(files []fs.FileInfo) int64 {
	var size int64
	for _, file := range files {
		size += file.Size()
	}
	return size
}

func (fc *FileCache) cleanCachedFileByLRU(ctx context.Context) error {
	files, err := fc.Files()
	if err != nil {
		return err
	}
	resize := totalSize(files) - fc.MaxSize
	if resize <= 0 {
		return nil
	}

	log := fc.Logger.WithField("strategy", "LRU")
	var firstErr error
	cleanedSize := int64(0)
	for _, file := range files {
		if cleanedSize >= resize {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fc.gcDelete(ctx, file.Name()); err != nil {
			log.WithError(err).Warnf("Failed to clean cache file %s", file.Name())
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		log.Debugf("Cleaned cached file %s", file.Name())
		cleanedSize += file.Size()
	}
	log.Infof("Cleaned %v(MB) cached files", byte2MB(cleanedSize))
	return firstErr
}

func (fc *FileCache) cleanCachedFiles(ctx context.Context) error {
	fc.Logger.Info("Start clearning cached files")

	if fc.lockFactory != nil {
		lock, err := fc.lockFactory.Lock(ctx, defaultLockKey)
		if err != nil {
			return err
		}
		defer lock.Unlock(ctx)
	}

	// Run LRU even if TTL failed on some entries, so one bad key cannot
	// stop the cache from being kept under MaxSize.
	ttlErr := fc.cleanCachedFileByTTL(ctx)
	lruErr := fc.cleanCachedFileByLRU(ctx)
	if ttlErr != nil {
		return ttlErr
	}
	return lruErr
}

// RunGC runs GC to clean old files
func (fc *FileCache) RunGC() {
	go func() {
		ticker := time.NewTicker(fc.CleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				if err := fc.cleanCachedFiles(ctx); err != nil {
					fc.Logger.WithError(err).Warn("Failed to clean cached files")
				}
				cancel()
			case <-fc.quit:
				return
			}
		}
	}()
}

// RunGC stops running GC
func (fc *FileCache) StopGC() {
	fc.stopGC.Do(func() {
		close(fc.quit)
	})
}
