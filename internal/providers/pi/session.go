package pi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/janekbaraniewski/openusage/internal/fileutil"
)

type piSessionHeader struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Timestamp string `json:"timestamp,omitempty"`
	CWD       string `json:"cwd,omitempty"`
	Title     string `json:"title,omitempty"`
	// ParentSession marks a fork, continuation, /tan clone or subagent: the
	// parent's session id, or its transcript path for subagents.
	ParentSession string `json:"parentSession,omitempty"`
}

// piOmpTitleRecord is OMP's fixed-width first line. OMP rewrites it in place
// whenever the session is retitled, so it is newer than the header's title.
type piOmpTitleRecord struct {
	Type    string `json:"type"`
	Version int    `json:"v"`
	Title   string `json:"title"`
}

type piMessageLine struct {
	Type      string         `json:"type"`
	ID        string         `json:"id,omitempty"`
	Timestamp string         `json:"timestamp,omitempty"`
	Message   *piMessageBody `json:"message,omitempty"`
	// OMP model_usage entries record side calls (auto-thinking, judges) with
	// provider, model and usage at the top level instead of in a message.
	Provider string   `json:"provider,omitempty"`
	Model    string   `json:"model,omitempty"`
	Usage    *piUsage `json:"usage,omitempty"`
}

type piMessageBody struct {
	Role     string   `json:"role,omitempty"`
	Model    string   `json:"model,omitempty"`
	Provider string   `json:"provider,omitempty"`
	Usage    *piUsage `json:"usage,omitempty"`
}

type piUsage struct {
	Input      *int64       `json:"input,omitempty"`
	Output     *int64       `json:"output,omitempty"`
	CacheRead  *int64       `json:"cacheRead,omitempty"`
	CacheWrite *int64       `json:"cacheWrite,omitempty"`
	Cost       *piUsageCost `json:"cost,omitempty"`
}

type piUsageCost struct {
	Total *float64 `json:"total,omitempty"`
}

type piSessionMeta struct {
	SessionID      string
	CWD            string
	WorkspaceLabel string
	HeaderTime     time.Time
	// ParentSession is set on forks, continuations, /tan clones and subagents.
	ParentSession string
	// Title is OMP's title record, else the header's title; may be empty.
	Title string
}

type piModelEntry struct {
	SessionID      string
	WorkspaceLabel string
	Provider       string
	Model          string
	Input          int64
	Output         int64
	CacheRead      int64
	CacheWrite     int64
	CostUSD        float64
	HasCost        bool
	Timestamp      time.Time
	// TurnKey identifies the turn across files: OMP copies a parent's entries,
	// ids and timestamps included, into a forked or continued session. Empty
	// when the line lacks an id or its own timestamp.
	TurnKey string
	// Inherited marks a copy of a parent's turn: in a file whose header names
	// a parent session, an entry dated before that header was copied in when
	// the file was created. /tan clones also zero the copy's cost.
	Inherited bool

	// transcript is the file the turn was read from; set by the scan, never cached.
	transcript *piTranscript
}

// readPiSessionFile parses one JSONL session file. Pi files start with a session
// header; OMP files may start with one versioned title record followed by the
// header. Any other prefix is skipped. Malformed message lines are dropped
// individually so partial corruption never poisons a whole session.
func readPiSessionFile(path string) ([]piModelEntry, piSessionMeta, error) {
	// OMP saves a transcript by writing a temp file and renaming it over the
	// original. On Windows that rename fails while another process holds the
	// file open without delete sharing, which os.Open does not grant.
	f, err := fileutil.OpenShared(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, piSessionMeta{}, nil
		}
		return nil, piSessionMeta{}, fmt.Errorf("pi: opening %s: %w", path, err)
	}
	defer f.Close()

	var fallback time.Time
	if info, statErr := f.Stat(); statErr == nil {
		fallback = info.ModTime().UTC()
	}

	lines := newLineReader(f)
	first, ok := lines.next()
	if !ok {
		return nil, piSessionMeta{}, nil
	}

	var header piSessionHeader
	var title piOmpTitleRecord
	if err := json.Unmarshal(first, &header); err != nil || header.Type != "session" {
		if err := json.Unmarshal(first, &title); err != nil ||
			title.Type != "title" ||
			title.Version != 1 {
			return nil, piSessionMeta{}, nil
		}
		second, ok := lines.next()
		if !ok {
			return nil, piSessionMeta{}, nil
		}
		header = piSessionHeader{}
		if err := json.Unmarshal(second, &header); err != nil || header.Type != "session" {
			return nil, piSessionMeta{}, nil
		}
	}

	meta := piSessionMeta{
		SessionID:      strings.TrimSpace(header.ID),
		CWD:            strings.TrimSpace(header.CWD),
		WorkspaceLabel: workspaceLabel(header.CWD),
		ParentSession:  strings.TrimSpace(header.ParentSession),
		Title:          strings.TrimSpace(title.Title),
	}
	if meta.Title == "" {
		meta.Title = strings.TrimSpace(header.Title)
	}
	if header.Timestamp != "" {
		if t, perr := time.Parse(time.RFC3339Nano, header.Timestamp); perr == nil {
			meta.HeaderTime = t.UTC()
		}
	}
	// Entries dated before a child's own header were copied from its parent.
	// Without a parent or a header time nothing is inherited: the zero time
	// is before every entry.
	var inheritedBefore time.Time
	if meta.ParentSession != "" {
		inheritedBefore = meta.HeaderTime
	}

	var out []piModelEntry
	for {
		raw, ok := lines.next()
		if !ok {
			break
		}
		// Tool results dominate transcript bytes; only assistant turns and
		// model_usage side calls carry usage.
		if !bytes.Contains(raw, assistantRole) && !bytes.Contains(raw, modelUsageType) {
			continue
		}
		var line piMessageLine
		if err := json.Unmarshal(raw, &line); err != nil {
			continue
		}
		var provider, model string
		var usage *piUsage
		switch line.Type {
		case "message":
			if line.Message == nil || line.Message.Role != "assistant" {
				continue
			}
			provider, model, usage = line.Message.Provider, line.Message.Model, line.Message.Usage
		case "model_usage":
			provider, model, usage = line.Provider, line.Model, line.Usage
		default:
			continue
		}
		if usage == nil {
			continue
		}

		entry := piModelEntry{
			SessionID:      meta.SessionID,
			WorkspaceLabel: meta.WorkspaceLabel,
			Provider:       strings.TrimSpace(provider),
			Model:          strings.TrimSpace(model),
			Input:          nonNegative(usage.Input),
			Output:         nonNegative(usage.Output),
			CacheRead:      nonNegative(usage.CacheRead),
			CacheWrite:     nonNegative(usage.CacheWrite),
		}
		if cost := usage.Cost; cost != nil && cost.Total != nil && *cost.Total >= 0 {
			entry.CostUSD = *cost.Total
			entry.HasCost = true
		}
		if entry.Input == 0 && entry.Output == 0 && entry.CacheRead == 0 && entry.CacheWrite == 0 &&
			(!entry.HasCost || entry.CostUSD == 0) {
			continue
		}

		if line.Timestamp != "" {
			if t, perr := time.Parse(time.RFC3339Nano, line.Timestamp); perr == nil {
				entry.Timestamp = t.UTC()
				entry.Inherited = entry.Timestamp.Before(inheritedBefore)
				if line.ID != "" {
					entry.TurnKey = line.ID + "@" + line.Timestamp
				}
			}
		}
		if entry.Timestamp.IsZero() {
			entry.Timestamp = fallback
		}

		out = append(out, entry)
	}
	return out, meta, nil
}

var (
	assistantRole  = []byte(`"assistant"`)
	modelUsageType = []byte(`"model_usage"`)
)

// lineReader yields JSONL lines of any length. OMP transcripts can hold
// multi-megabyte tool results; a capped scanner would stop at the first one
// and silently drop every later turn in the file.
type lineReader struct {
	reader *bufio.Reader
	line   []byte
}

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{reader: bufio.NewReaderSize(r, 64*1024)}
}

// next returns the following line without its terminator. The slice is
// reused by the next call. A read error ends the stream like EOF.
func (l *lineReader) next() ([]byte, bool) {
	l.line = l.line[:0]
	for {
		chunk, err := l.reader.ReadSlice('\n')
		l.line = append(l.line, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil && len(l.line) == 0 {
			return nil, false
		}
		return bytes.TrimRight(l.line, "\r\n"), true
	}
}

func workspaceLabel(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return ""
	}
	clean := filepath.ToSlash(cwd)
	clean = strings.TrimRight(clean, "/")
	if clean == "" {
		return ""
	}
	idx := strings.LastIndex(clean, "/")
	if idx < 0 {
		return clean
	}
	return clean[idx+1:]
}

func nonNegative(p *int64) int64 {
	if p == nil {
		return 0
	}
	if *p < 0 {
		return 0
	}
	return *p
}
