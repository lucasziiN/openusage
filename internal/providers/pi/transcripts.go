package pi

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// piTranscript is one parsed session file, the unit the parse cache stores.
type piTranscript struct {
	// Size and ModTime (Unix nanoseconds) identify the version that was parsed.
	Size    int64
	ModTime int64
	Meta    piSessionMeta
	Entries []piModelEntry

	path string // resolved path, the cache key
	// root is the top-most session whose artifacts directory holds this
	// transcript, or the transcript itself.
	root *piTranscript
}

// readAllSessions returns every recorded turn once. OMP starts a fork, a
// continuation and a /tan clone with a verbatim copy of the parent's entries,
// ids and timestamps included, and a /tan clone also zeroes the copies' cost.
// The clone lives in the parent's artifacts directory, which sorts before the
// parent file, so keeping whichever copy the walk meets first would bill the
// parent's turns at $0. Copies are resolved per turn instead: the copy
// recorded where the turn happened beats inherited ones, then the higher
// cost, then walk order. An inherited copy survives only when it is the last
// one left (its parent was deleted); one without a turn key cannot be matched
// to its original and is dropped.
func readAllSessions(ctx context.Context, dirs []string) ([]piModelEntry, error) {
	transcripts, err := scanTranscripts(ctx, dirs)
	if err != nil {
		return nil, err
	}
	var out []piModelEntry
	kept := make(map[string]int)
	for _, transcript := range transcripts {
		for _, entry := range transcript.Entries {
			if entry.TurnKey == "" {
				if !entry.Inherited {
					out = append(out, entry)
				}
				continue
			}
			index, dup := kept[entry.TurnKey]
			if !dup {
				kept[entry.TurnKey] = len(out)
				out = append(out, entry)
				continue
			}
			if prefersCopy(entry, out[index]) {
				out[index] = entry
			}
		}
	}
	return out, nil
}

// prefersCopy reports whether candidate is a better copy of a turn than kept.
func prefersCopy(candidate, kept piModelEntry) bool {
	if candidate.Inherited != kept.Inherited {
		return !candidate.Inherited
	}
	return candidate.CostUSD > kept.CostUSD
}

// scanTranscripts returns every .jsonl transcript under dirs in walk order.
// The OMP statusline rescans every minute per running instance, so a file
// whose size and modification time match the parse cache is not reopened.
// Cache failures only cost a re-parse.
func scanTranscripts(ctx context.Context, dirs []string) ([]*piTranscript, error) {
	cachePath := parseCachePath()
	cached := loadParseCache(cachePath)
	current := make(map[string]*piTranscript, len(cached))
	var transcripts []*piTranscript
	roots := make([]string, 0, len(dirs))
	changed := false
	for _, dir := range dirs {
		root := canonicalPath(dir)
		roots = append(roots, root)
		walkErr := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || filepath.Ext(path) != ".jsonl" {
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			key := transcriptKey(dir, root, path, entry)
			if _, dup := current[key]; dup {
				return nil
			}
			// os.Stat, not the walk's entry info: Windows documents that directory
			// listings may report out-of-date attributes, and a stale size and
			// time would keep serving an old parse of a live transcript.
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			transcript := cached[key]
			size, modTime := info.Size(), info.ModTime().UnixNano()
			if transcript == nil || transcript.Size != size || transcript.ModTime != modTime {
				entries, meta, err := readPiSessionFile(path)
				switch {
				case err == nil:
					// Stat before reading: a write racing the read leaves a key
					// older than the content, so the next scan parses again.
					transcript = &piTranscript{Size: size, ModTime: modTime, Meta: meta, Entries: entries}
					changed = true
				case transcript == nil:
					return nil
				}
				// A file that cannot be opened right now keeps its last parse.
			}
			transcript.path = key
			current[key] = transcript
			transcripts = append(transcripts, transcript)
			return nil
		})
		if walkErr != nil {
			return nil, walkErr
		}
	}
	for key, transcript := range cached {
		if _, seen := current[key]; seen {
			continue
		}
		if withinAny(key, roots) {
			changed = true // deleted since the last scan
			continue
		}
		// Another account's sessions directory; keep it for that scan.
		current[key] = transcript
	}
	if changed {
		saveParseCache(cachePath, current)
	}
	linkTranscripts(transcripts)
	return transcripts, nil
}

// transcriptKey resolves a walked file's path. WalkDir does not descend into
// symlinked directories, so for a regular file joining the resolved root with
// the relative path is exact and spares filepath.EvalSymlinks' per-component
// lookups, which add up on Windows across hundreds of transcripts.
func transcriptKey(dir, root, path string, entry fs.DirEntry) string {
	if entry.Type()&fs.ModeSymlink == 0 {
		if rel, err := filepath.Rel(dir, path); err == nil {
			return filepath.Join(root, rel)
		}
	}
	return canonicalPath(path)
}

func canonicalPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		if abs, err := filepath.Abs(resolved); err == nil {
			return abs
		}
		return resolved
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

func withinAny(path string, roots []string) bool {
	for _, root := range roots {
		rel, err := filepath.Rel(root, path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// linkTranscripts points every transcript at its root session and every
// entry at its transcript. OMP keeps subagent transcripts and /tan clones in
// the spawning session's artifacts directory (its file path without .jsonl),
// nested as deep as subagents spawn subagents; the top-most session that owns
// such a directory is the one the user started.
func linkTranscripts(transcripts []*piTranscript) {
	sessions := make(map[string]*piTranscript, len(transcripts))
	for _, transcript := range transcripts {
		if transcript.Meta.SessionID != "" {
			sessions[strings.TrimSuffix(transcript.path, ".jsonl")] = transcript
		}
	}
	for _, transcript := range transcripts {
		transcript.root = transcript
		for dir := filepath.Dir(transcript.path); ; {
			if owner, ok := sessions[dir]; ok {
				transcript.root = owner
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
		for i := range transcript.Entries {
			transcript.Entries[i].transcript = transcript
		}
	}
}
