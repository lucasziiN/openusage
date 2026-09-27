package pi

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"os"
	"path/filepath"

	"github.com/janekbaraniewski/openusage/internal/fileutil"
)

// parseCacheVersion names and stamps the parse cache. Bump it whenever
// piTranscript, piSessionMeta or piModelEntry change what a parse records, so
// stale results are parsed afresh instead of decoding into the wrong shape.
const parseCacheVersion = 1

// parseCachePath locates the parse cache; "" disables it. Tests redirect it
// so they never read or overwrite the user's cache.
var parseCachePath = func() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "openusage", fmt.Sprintf("pi-sessions-v%d.gob", parseCacheVersion))
}

type parseCacheFile struct {
	Version     int
	Transcripts map[string]*piTranscript // by resolved path
}

// loadParseCache returns the cached transcripts, or nil when the cache is
// missing, unreadable or from another version.
func loadParseCache(path string) map[string]*piTranscript {
	if path == "" {
		return nil
	}
	data, err := fileutil.ReadFile(path)
	if err != nil {
		return nil
	}
	var stored parseCacheFile
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&stored); err != nil ||
		stored.Version != parseCacheVersion {
		return nil
	}
	return stored.Transcripts
}

// saveParseCache replaces the cache atomically: several OMP instances scan
// concurrently, and a reader must never decode a half-written file. Failures
// are ignored; the next scan parses again.
func saveParseCache(path string, transcripts map[string]*piTranscript) {
	if path == "" {
		return
	}
	var data bytes.Buffer
	if err := gob.NewEncoder(&data).Encode(parseCacheFile{
		Version: parseCacheVersion, Transcripts: transcripts,
	}); err != nil {
		return
	}
	_ = fileutil.WriteFileAtomic(path, data.Bytes(), 0o600)
}
