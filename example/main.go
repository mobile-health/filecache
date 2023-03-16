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
