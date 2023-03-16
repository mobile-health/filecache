package filecache

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	defaultMaxSize         = 1024 * 1024 * 1024 // 1GB
	defaultMaxTTL          = 4 * time.Hour      // 4 hours
	defaultBaseDir         = "filecache"
	defaultCleanupInterval = 5 * time.Minute
	defaultLockKey         = "lock_filecache"
	defaultDirFileMode     = os.FileMode(0777)
)

var (
	errKeyExisted = errors.New("key existed")
)

type Config struct {
	BaseDir         string
	TempDir         string
	MaxSize         int64
	MaxTTL          time.Duration
	CleanupInterval time.Duration
	LogLevel        logrus.Level
}

type ILock interface {
	Unlock()
}

type ILockFatory interface {
	Lock(key string) (ILock, error)
}

type FileCache struct {
	Config
	lockFactory ILockFatory
	quit        chan bool
	Logger      *logrus.Logger
}

func ensureDir(dir string) (string, error) {
	if absdir, err := filepath.Abs(dir); err != nil {
		return "", err
	} else {
		dir = absdir
	}
	if err := os.MkdirAll(dir, os.FileMode(0777)); err != nil {
		panic(err)
	}
	return dir, nil
}

func New(config Config, lockFactory ILockFatory) *FileCache {
	fc := &FileCache{Config: config, lockFactory: lockFactory, quit: make(chan bool)}
	if len(fc.BaseDir) == 0 {
		fc.BaseDir = defaultBaseDir
	}
	if dir, err := ensureDir(fc.BaseDir); err != nil {
		panic(err)
	} else {
		fc.BaseDir = dir
	}

	if len(fc.TempDir) == 0 {
		fc.TempDir = os.TempDir()
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

func (f *FileCache) absFilePath(key string) string {
	return filepath.Join(f.BaseDir, key)
}

func (f *FileCache) hasFile(key string) (string, error) {
	absFilePath := f.absFilePath(key)
	if err := checkFileExist(absFilePath); err != nil {
		return absFilePath, err
	}
	return absFilePath, nil
}

// Read returns an IO stream of file reader
func (f *FileCache) Read(key string) (io.ReadCloser, error) {
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
func (f *FileCache) Write(key string, r io.Reader) error {
	if f.lockFactory != nil {
		lock, err := f.lockFactory.Lock(keylock(key))
		if err != nil {
			return err
		}
		defer lock.Unlock()
	}

	absFilePath, err := f.hasFile(key)
	if err == nil {
		return errKeyExisted
	}

	tmp, err := os.CreateTemp(f.TempDir, "filecachetmp-")
	if err != nil {
		return err
	}
	defer tmp.Close()
	defer os.Remove(tmp.Name())

	if _, err := io.Copy(tmp, r); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), absFilePath); err != nil {
		return err
	}
	return nil
}

func (f *FileCache) Delete(key string) error {
	if f.lockFactory != nil {
		lock, err := f.lockFactory.Lock(keylock(key))
		if err != nil {
			return err
		}
		defer lock.Unlock()
	}

	absFilePath, err := f.hasFile(key)
	if err != nil {
		return err
	}
	return os.Remove(absFilePath)
}

func (f *FileCache) Empty() error {
	if f.lockFactory != nil {
		lock, err := f.lockFactory.Lock(defaultLockKey)
		if err != nil {
			return err
		}
		defer lock.Unlock()
	}

	if err := os.RemoveAll(f.TempDir); err != nil {
		return err
	}
	if err := os.RemoveAll(f.BaseDir); err != nil {
		return err
	}
	return nil
}

func (fc FileCache) touch(key string, ts time.Time) error {
	if ts.IsZero() {
		ts = time.Now()
	}
	return os.Chtimes(fc.absFilePath(key), ts, ts)
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
	sort.Slice(list, func(i, j int) bool { return list[i].ModTime().Unix() < list[j].ModTime().Unix() })
	return list, nil
}

func (fc *FileCache) cleanCachedFileByTTL() error {
	files, err := fc.Files()
	if err != nil {
		return nil
	}

	count := 0
	for _, file := range files {
		ttl := time.Since(file.ModTime())
		if ttl > fc.MaxTTL {
			if err := fc.Delete(file.Name()); err != nil {
				return err
			}
			count++
			fc.Logger.WithField("strategy", "TTL").Debugf("Cleaned cache file %s", file.Name())
		}
	}
	fc.Logger.WithField("strategy", "TTL").Infof("Cleaned %v files", count)
	return nil
}

func (fc *FileCache) Size() (int64, error) {
	var size int64
	err := filepath.Walk(fc.BaseDir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return err
	})
	return size, err

}

func (fc *FileCache) cleanCachedFileByLRU() error {
	curSize, err := fc.Size()
	if err != nil {
		return err
	}
	resize := curSize - fc.MaxSize
	if resize > 0 {
		files, err := fc.Files()
		if err != nil {
			return nil
		}

		cleanedSize := int64(0)
		for _, file := range files {
			if err := fc.Delete(file.Name()); err != nil {
				return err
			} else {
				fc.Logger.WithField("strategy", "LRU").Debugf("Cleaned cached file %s", file.Name())

				cleanedSize += file.Size()
				if cleanedSize >= resize {
					break
				}
			}
		}
		fc.Logger.WithField("strategy", "LRU").Infof("Cleaned %v(MB) cached files", byte2MB(cleanedSize))
	}
	return nil
}

func (fc *FileCache) cleanCachedFiles() error {
	fc.Logger.Info("Start clearning cached files")

	if fc.lockFactory != nil {
		lock, err := fc.lockFactory.Lock(defaultLockKey)
		if err != nil {
			return err
		}
		defer lock.Unlock()
	}

	if err := fc.cleanCachedFileByTTL(); err != nil {
		return err
	}

	if err := fc.cleanCachedFileByLRU(); err != nil {
		return err
	}
	return nil
}

// RunGC runs GC to clean old files
func (fc *FileCache) RunGC() {
	go func() {
		ticker := time.NewTicker(fc.CleanupInterval)
		for {
			<-ticker.C
			select {
			case <-ticker.C:
				if err := fc.cleanCachedFiles(); err != nil {
					fc.Logger.WithError(err).Warn("Failed to clean cached files")
				}
			case <-fc.quit:
				return
			}
		}
	}()
}

// RunGC stops running GC
func (fc *FileCache) StopGC() {
	close(fc.quit)
}
