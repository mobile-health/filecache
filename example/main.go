package main

import (
	"bytes"
	"io"
	"time"

	"github.com/mobile-health/filecache"
)

func sampleReader(s string) io.Reader {
	return bytes.NewReader([]byte(s))
}

func main() {
	fc := filecache.New(filecache.Config{
		BaseDir:         "filecache",
		TempDir:         "tmp",
		MaxTTL:          60 * time.Second,
		MaxSize:         10 * 1024 * 1024,
		CleanupInterval: 10 * time.Second,
	}, nil)
	defer fc.Empty()

	fc.RunGC()
	defer fc.StopGC()

	fc.Write("key", sampleReader("ABC"))
	fc.Read("key")
	fc.Delete("key")
}
