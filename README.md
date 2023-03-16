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
	"io"

	"github.com/mobile-health/filecache"
)

func sampleReader(s string) io.Reader {
	return bytes.NewReader([]byte(s))
}

func main() {
	fc := filecache.New(filecache.Config{TempDir: "tmp"}, nil)
	defer fc.Empty()

	fc.RunGC()
	defer fc.StopGC()

	fc.Write("key", sampleReader("ABC"))
	fc.Read("key")
	fc.Delete("key")
}

```