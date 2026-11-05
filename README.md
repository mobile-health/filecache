# FileCache

**FileCache** is a simple, persistent key-value store written in the Go language.   

# Main features

- Key-Value store directly on disk
- Write and read using buffer streams to avoid using up RAM memory
- Automatically cleans old files using TTL and LRU strategies
- Compatible with distributed systems.


# Errors

| Error | Returned by | Meaning |
|---|---|---|
| `fs.ErrNotExist` | `Read`, `Delete` | Key is not cached |
| `filecache.ErrKeyExisted` | `Write` | Key is already cached |
| `filecache.ErrLocked` | `Read` | Key is locked by a `Write` or `Delete` in progress |
| `filecache.ErrInvalidKey` | all key methods | Key is empty, `.`, `..`, or contains `/`, `\` or NUL |

Check them with `errors.Is`. GC failures are logged at warn level by default (`Config.LogLevel`).

# Usage


```
package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"time"

	"github.com/mobile-health/filecache"
)

func sampleReader(s string) io.Reader {
	return bytes.NewReader([]byte(s))
}

func main() {
	ctx := context.Background()

	fc := filecache.New(filecache.Config{
		BaseDir:         "filecache",
		TempDir:         "tmp",
		MaxTTL:          60 * time.Second,
		MaxSize:         10 * 1024 * 1024,
		CleanupInterval: 10 * time.Second,
	}, nil)
	defer fc.Empty(ctx)

	fc.RunGC()
	defer fc.StopGC()

	if err := fc.Write(ctx, "key", sampleReader("ABC")); err != nil && !errors.Is(err, filecache.ErrKeyExisted) {
		panic(err)
	}

	r, err := fc.Read(ctx, "key")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// cache miss
	case errors.Is(err, filecache.ErrLocked):
		// key is being written or deleted: retry or treat as a miss
	case err != nil:
		panic(err)
	default:
		defer r.Close()
	}

	fc.Delete(ctx, "key")
}

```
