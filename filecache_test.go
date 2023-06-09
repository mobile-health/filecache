package filecache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
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

func TestWriteReadEmpty(t *testing.T) {
	ctx := context.Background()

	fc := New(Config{TempDir: "tmp"}, nil)
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
	if existDir(fc.BaseDir) || existDir(fc.TempDir) {
		t.Fatal("failed to empty")
	}
}

func TestWrite(t *testing.T) {
	ctx := context.Background()

	fc := New(Config{TempDir: "tmp"}, nil)
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

	fc := New(Config{TempDir: "tmp"}, nil)
	defer fc.Empty(ctx)

	if err := fc.Write(ctx, "key", sampleReader("ABC")); err != nil {
		t.Fatal(err)
	}

	if r, err := fc.Read(ctx, "key"); err != nil {
		t.Fatal(err)
	} else {
		var buf bytes.Buffer
		io.Copy(&buf, r)
		if buf.String() != "ABC" {
			t.Fatal("file not matched")
		}
	}
	mt1, err := getModTime(fc.absFilePath("key"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	fc.Read(ctx, "key")
	mt2, _ := getModTime(fc.absFilePath("key"))
	if mt2.Unix() <= mt1.Unix() {
		t.Fatal("mod time must be changed")
	}
}

func TestFiles(t *testing.T) {
	ctx := context.Background()

	fc := New(Config{TempDir: "tmp"}, nil)
	defer fc.Empty(ctx)

	if err := fc.Write(ctx, "key1", sampleReader("ABC2")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second * 1)

	if err := fc.Write(ctx, "key2", sampleReader("ABC3")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(time.Second * 1)
	if err := fc.Write(ctx, "akey", sampleReader("ABC3")); err != nil {
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

	fc := New(Config{TempDir: "tmp"}, nil)
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

	fc := New(Config{TempDir: "tmp"}, lockFactory)
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

	fc := New(Config{TempDir: "tmp", MaxTTL: 1 * time.Second}, nil)
	defer fc.Empty(ctx)

	if err := fc.Write(ctx, "key1", sampleReader("ABC1")); err != nil {
		t.Fatal(err)
	}
	if err := fc.Write(ctx, "key2", sampleReader("ABC2")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if err := fc.Write(ctx, "key3", sampleReader("ABC3")); err != nil {
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

	fc := New(Config{TempDir: "tmp", MaxSize: int64(size)*2 - 1}, nil)
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
