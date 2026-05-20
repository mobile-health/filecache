# FileCache

**FileCache** is a simple, persistent key-value store written in the Go language.   

# Main features

- Key-Value store directly on disk
- Write and read using buffer streams to avoid using up RAM memory
- Automatically cleans old files using TTL and LRU strategies
- Compatible with distributed systems.


# Usage


```
package main

import (
	"bytes"
	"context"
	"io"
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

	fc.Write(ctx, "key", sampleReader("ABC"))
	fc.Read(ctx, "key")
	fc.Delete(ctx, "key")
}

```
