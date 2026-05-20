package filecache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func sampleReader(s string) io.Reader {
	return bytes.NewReader([]byte(s))
}

func existDir(dir string) bool {
	fs, err := os.Stat(dir)
	if err != nil {
		return false
	}
	return fs.IsDir()
}

func newTestFileCache(t *testing.T, config Config, lockFactory ILockFatory) *FileCache {
	t.Helper()

	root := t.TempDir()
	if config.BaseDir == "" {
		config.BaseDir = filepath.Join(root, "cache")
	}
	if config.TempDir == "" {
		config.TempDir = filepath.Join(root, "tmp")
	}
	return New(config, lockFactory)
}

func TestWriteReadEmpty(t *testing.T) {
	ctx := context.Background()

	fc := newTestFileCache(t, Config{}, nil)
	defer fc.Empty(ctx)

	if err := fc.Write(ctx, "key", sampleReader("ABC")); err != nil {
		t.Fatal(err)
	}

	r, err := fc.Read(ctx, "key")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "ABC" {
		t.Fatal("data not match")
	}

	if err := fc.Empty(ctx); err != nil {
		t.Fatal(err)
	}
	if !existDir(fc.BaseDir) || !existDir(fc.TempDir) {
		t.Fatal("empty must preserve cache directories for reuse")
	}
	files, err := fc.Files()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatal("empty must remove cached files")
	}
}

func TestWrite(t *testing.T) {
	ctx := context.Background()

	fc := newTestFileCache(t, Config{}, nil)
	defer fc.Empty(ctx)

	if err := fc.Write(ctx, "key", sampleReader("ABC")); err != nil {
		t.Fatal(err)
	}

	if err := fc.Write(ctx, "key", sampleReader("ABC")); err == nil {
		t.Fatal("must duplicate error")
	}
}

func getModTime(path string) (time.Time, error) {
	fs, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return fs.ModTime(), nil
}

func TestRead(t *testing.T) {
	ctx := context.Background()

	fc := newTestFileCache(t, Config{}, nil)
	defer fc.Empty(ctx)

	if err := fc.Write(ctx, "key", sampleReader("ABC")); err != nil {
		t.Fatal(err)
	}

	if r, err := fc.Read(ctx, "key"); err != nil {
		t.Fatal(err)
	} else {
		defer r.Close()
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, r); err != nil {
			t.Fatal(err)
		}
		if buf.String() != "ABC" {
			t.Fatal("file not matched")
		}
	}
	mt1, err := getModTime(fc.absFilePath("key"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	r, err := fc.Read(ctx, "key")
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	mt2, _ := getModTime(fc.absFilePath("key"))
	if mt2.Unix() <= mt1.Unix() {
		t.Fatal("mod time must be changed")
	}
}

func TestFiles(t *testing.T) {
	ctx := context.Background()

	fc := newTestFileCache(t, Config{}, nil)
	defer fc.Empty(ctx)

	if err := fc.Write(ctx, "key1", sampleReader("ABC2")); err != nil {
		t.Fatal(err)
	}
	if err := fc.Write(ctx, "key2", sampleReader("ABC3")); err != nil {
		t.Fatal(err)
	}
	if err := fc.Write(ctx, "akey", sampleReader("ABC3")); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := fc.touch("key1", now.Add(-3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := fc.touch("key2", now.Add(-2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := fc.touch("akey", now.Add(-1*time.Second)); err != nil {
		t.Fatal(err)
	}

	files, err := fc.Files()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatal("must have 3 files")
	}

	for _, file := range files {
		t.Log(file.Name(), file.ModTime().UnixNano())
	}

	if files[0].Name() != "key1" || files[1].Name() != "key2" || files[2].Name() != "akey" {
		t.Fatal("must ordered by mod time")
	}
}

func TestDelete(t *testing.T) {
	ctx := context.Background()

	fc := newTestFileCache(t, Config{}, nil)
	defer fc.Empty(ctx)

	fc.Write(ctx, "key1", sampleReader("ABC1"))
	if err := fc.Delete(ctx, "key1"); err != nil {
		t.Fatal(err)
	}
	if _, err := fc.Read(ctx, "key1"); err == nil {
		t.Fatal("key1 must be deleted")
	}
}

type Lock struct {
	key   string
	locks map[string]bool
	mutex *sync.Mutex
}

func (l *Lock) Unlock(ctx context.Context) {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	delete(l.locks, l.key)
}

type LockFactory struct {
	locks map[string]bool
	mutex *sync.Mutex
}

func (fac *LockFactory) Lock(ctx context.Context, key string) (ILock, error) {
	fac.mutex.Lock()
	defer fac.mutex.Unlock()

	if _, ok := fac.locks[key]; ok {
		return nil, errors.New("failed to lock")
	}
	fac.locks[key] = true
	return &Lock{
		key:   key,
		locks: fac.locks,
		mutex: fac.mutex,
	}, nil
}

func (fac *LockFactory) Has(ctx context.Context, key string) bool {
	fac.mutex.Lock()
	defer fac.mutex.Unlock()

	_, ok := fac.locks[key]
	return ok
}

func TestLocker(t *testing.T) {
	lockFactory := &LockFactory{locks: map[string]bool{}, mutex: &sync.Mutex{}}
	ctx := context.Background()

	fc := newTestFileCache(t, Config{}, lockFactory)
	defer fc.Empty(ctx)

	if _, err := fc.lockFactory.Lock(ctx, "key1"); err != nil {
		t.Fatal(err)
	}
	if _, err := fc.lockFactory.Lock(ctx, "key1"); err == nil {
		t.Fatal("must be locked")
	}
}

func TestCleanCachedFileByTTL(t *testing.T) {
	ctx := context.Background()

	fc := newTestFileCache(t, Config{MaxTTL: time.Second}, nil)
	defer fc.Empty(ctx)

	if err := fc.Write(ctx, "key1", sampleReader("ABC1")); err != nil {
		t.Fatal(err)
	}
	if err := fc.Write(ctx, "key2", sampleReader("ABC2")); err != nil {
		t.Fatal(err)
	}
	if err := fc.Write(ctx, "key3", sampleReader("ABC3")); err != nil {
		t.Fatal(err)
	}
	if err := fc.touch("key1", time.Now().Add(-2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := fc.touch("key2", time.Now().Add(-2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := fc.cleanCachedFileByTTL(ctx); err != nil {
		t.Fatal(err)
	} else {
		files, err := fc.Files()
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 1 {
			t.Fatal("must have 1 files")
		}
		if files[0].Name() != "key3" {
			t.Fatal("must be the last key")
		}
	}
}

func TestCleanCachedFileByLRU(t *testing.T) {
	ctx := context.Background()
	data := "bytesample"
	size := len(data)

	fc := newTestFileCache(t, Config{MaxSize: int64(size)*2 - 1}, nil)
	defer fc.Empty(ctx)

	if err := fc.Write(ctx, "key1", sampleReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := fc.Write(ctx, "key2", sampleReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := fc.Write(ctx, "key3", sampleReader(data)); err != nil {
		t.Fatal(err)
	}
	fc.touch("key1", time.Now().Add(-time.Minute))
	fc.touch("key3", time.Now().Add(-time.Minute))

	if err := fc.cleanCachedFileByLRU(ctx); err != nil {
		t.Fatal(err)
	} else {
		files, err := fc.Files()
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 1 {
			t.Fatal("must have 1 files")
		}
		if files[0].Name() != "key2" {
			t.Fatal("must be the key2")
		}
	}
}

func TestDefaultTempDirIsCacheOwned(t *testing.T) {
	ctx := context.Background()
	baseDir := filepath.Join(t.TempDir(), "cache")

	fc := New(Config{BaseDir: baseDir}, nil)
	defer fc.Empty(ctx)

	if fc.TempDir != filepath.Join(fc.BaseDir, ".tmp") {
		t.Fatalf("default temp dir = %q, want cache-owned temp dir", fc.TempDir)
	}
	if err := fc.Write(ctx, "key", sampleReader("ABC")); err != nil {
		t.Fatal(err)
	}
	files, err := fc.Files()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name() != "key" {
		t.Fatalf("Files() = %v, want only cached files", files)
	}
}

func TestEmptyPreservesSharedTempDir(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	sharedTempDir := filepath.Join(root, "shared-temp")
	if err := os.MkdirAll(sharedTempDir, defaultDirFileMode); err != nil {
		t.Fatal(err)
	}

	sentinelPath := filepath.Join(sharedTempDir, "sentinel.txt")
	if err := os.WriteFile(sentinelPath, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	leftoverTempPath := filepath.Join(sharedTempDir, tempFilePrefix+"leftover")
	if err := os.WriteFile(leftoverTempPath, []byte("remove"), 0600); err != nil {
		t.Fatal(err)
	}

	fc := New(Config{
		BaseDir: filepath.Join(root, "cache"),
		TempDir: sharedTempDir,
	}, nil)
	if err := fc.Write(ctx, "key", sampleReader("ABC")); err != nil {
		t.Fatal(err)
	}

	if err := fc.Empty(ctx); err != nil {
		t.Fatal(err)
	}
	if !existDir(sharedTempDir) {
		t.Fatal("empty must not remove the configured temp directory")
	}
	if _, err := os.Stat(sentinelPath); err != nil {
		t.Fatalf("empty removed a non-cache temp file: %v", err)
	}
	if _, err := os.Stat(leftoverTempPath); !os.IsNotExist(err) {
		t.Fatalf("empty must remove leftover cache temp files, got %v", err)
	}
}

func TestUnsafeKeysRejected(t *testing.T) {
	ctx := context.Background()
	fc := newTestFileCache(t, Config{}, nil)
	defer fc.Empty(ctx)

	for _, key := range []string{"", ".", "..", "../escape", "nested/key", `nested\key`, "/absolute"} {
		t.Run(key, func(t *testing.T) {
			if err := fc.Write(ctx, key, sampleReader("ABC")); !errors.Is(err, errInvalidKey) {
				t.Fatalf("Write(%q) error = %v, want %v", key, err, errInvalidKey)
			}
			if _, err := fc.Read(ctx, key); !errors.Is(err, errInvalidKey) {
				t.Fatalf("Read(%q) error = %v, want %v", key, err, errInvalidKey)
			}
			if err := fc.Delete(ctx, key); !errors.Is(err, errInvalidKey) {
				t.Fatalf("Delete(%q) error = %v, want %v", key, err, errInvalidKey)
			}
			if err := fc.touch(key, time.Now()); !errors.Is(err, errInvalidKey) {
				t.Fatalf("touch(%q) error = %v, want %v", key, err, errInvalidKey)
			}
			if fc.Has(key) {
				t.Fatalf("Has(%q) = true, want false", key)
			}
		})
	}

	if _, err := os.Stat(filepath.Join(filepath.Dir(fc.BaseDir), "escape")); !os.IsNotExist(err) {
		t.Fatalf("unsafe key wrote outside the cache: %v", err)
	}
}

type blockingReader struct {
	reader  io.Reader
	once    sync.Once
	started chan<- struct{}
	release <-chan struct{}
}

func (r *blockingReader) Read(p []byte) (int, error) {
	r.once.Do(func() {
		r.started <- struct{}{}
		<-r.release
	})
	return r.reader.Read(p)
}

func TestConcurrentWriteDoesNotOverwriteDuplicateKey(t *testing.T) {
	ctx := context.Background()
	fc := newTestFileCache(t, Config{}, nil)
	defer fc.Empty(ctx)

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	errs := make(chan error, 2)

	for _, data := range []string{"first", "second"} {
		go func(data string) {
			errs <- fc.Write(ctx, "key", &blockingReader{
				reader:  sampleReader(data),
				started: started,
				release: release,
			})
		}(data)
	}

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for concurrent writes to start")
		}
	}
	close(release)

	successes := 0
	duplicates := 0
	for i := 0; i < 2; i++ {
		err := <-errs
		switch {
		case err == nil:
			successes++
		case errors.Is(err, errKeyExisted):
			duplicates++
		default:
			t.Fatal(err)
		}
	}
	if successes != 1 || duplicates != 1 {
		t.Fatalf("successes=%d duplicates=%d, want 1 success and 1 duplicate", successes, duplicates)
	}

	r, err := fc.Read(ctx, "key")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "first" && got != "second" {
		t.Fatalf("stored value = %q, want one complete write", got)
	}
}

func TestFilesSortsByFullModTime(t *testing.T) {
	ctx := context.Background()
	fc := newTestFileCache(t, Config{}, nil)
	defer fc.Empty(ctx)

	if err := fc.Write(ctx, "newer", sampleReader("ABC")); err != nil {
		t.Fatal(err)
	}
	if err := fc.Write(ctx, "older", sampleReader("ABC")); err != nil {
		t.Fatal(err)
	}
	base := time.Unix(100, 0)
	if err := fc.touch("newer", base.Add(200*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := fc.touch("older", base.Add(100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}

	files, err := fc.Files()
	if err != nil {
		t.Fatal(err)
	}
	if files[0].Name() != "older" || files[1].Name() != "newer" {
		t.Fatalf("files order = %s, %s; want older, newer", files[0].Name(), files[1].Name())
	}
}

func TestCleanCachedFileByTTLReturnsFilesError(t *testing.T) {
	basePath := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(basePath, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	fc := &FileCache{Config: Config{BaseDir: basePath, MaxTTL: time.Second}}

	if err := fc.cleanCachedFileByTTL(context.Background()); err == nil {
		t.Fatal("expected Files error")
	}
}

func TestCleanCachedFileByLRUReturnsFilesError(t *testing.T) {
	basePath := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(basePath, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	fc := &FileCache{Config: Config{BaseDir: basePath, MaxSize: -1}}

	if err := fc.cleanCachedFileByLRU(context.Background()); err == nil {
		t.Fatal("expected Files error")
	}
}

func TestStopGCIsSafeBeforeFirstTickAndWhenCalledTwice(t *testing.T) {
	fc := newTestFileCache(t, Config{CleanupInterval: time.Hour}, nil)

	fc.RunGC()
	fc.StopGC()
	fc.StopGC()
}

func TestRunGCCleansExpiredFiles(t *testing.T) {
	ctx := context.Background()
	fc := newTestFileCache(t, Config{
		MaxTTL:          time.Nanosecond,
		CleanupInterval: 20 * time.Millisecond,
	}, nil)
	defer fc.StopGC()
	defer fc.Empty(ctx)

	if err := fc.Write(ctx, "key", sampleReader("ABC")); err != nil {
		t.Fatal(err)
	}
	if err := fc.touch("key", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}

	fc.RunGC()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if !fc.Has("key") {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for GC to clean expired file")
		}
	}
}
