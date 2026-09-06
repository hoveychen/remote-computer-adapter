package trustedstate

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const NativePageBytes = 32 << 10

var noteFilename = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}-[0-9]{2}-[0-9]{2}-[a-z0-9-]{1,80}\.md$`)

const notePrefix = "extensions/ad_hoc/notes/"

type MemoryEntry struct {
	Path      string `json:"path"`
	EntryType string `json:"entry_type"`
}
type MemoryListRequest struct {
	Path       *string `json:"path"`
	Cursor     *string `json:"cursor"`
	MaxResults int     `json:"max_results"`
}
type MemoryListResult struct {
	snapshot   uint64
	Path       *string       `json:"path"`
	Entries    []MemoryEntry `json:"entries"`
	NextCursor *string       `json:"next_cursor"`
	Truncated  bool          `json:"truncated"`
}
type MemoryReadRequest struct {
	ExpectedSequence *uint64 `json:"expected_sequence,omitempty"`
	Path             string  `json:"path"`
	LineOffset       int     `json:"line_offset"`
	MaxLines         *int    `json:"max_lines"`
}
type MemoryReadResult struct {
	snapshot        uint64
	Path            string `json:"path"`
	StartLineNumber int    `json:"start_line_number"`
	Content         string `json:"content"`
	Truncated       bool   `json:"truncated"`
}
type MemoryMatchMode struct {
	Type      string `json:"type"`
	LineCount int    `json:"line_count,omitempty"`
}
type MemorySearchRequest struct {
	Queries       []string        `json:"queries"`
	MatchMode     MemoryMatchMode `json:"match_mode"`
	Path          *string         `json:"path"`
	Cursor        *string         `json:"cursor"`
	ContextLines  int             `json:"context_lines"`
	CaseSensitive bool            `json:"case_sensitive"`
	Normalized    bool            `json:"normalized"`
	MaxResults    int             `json:"max_results"`
}
type MemoryMatch struct {
	Path                   string   `json:"path"`
	MatchLineNumber        int      `json:"match_line_number"`
	ContentStartLineNumber int      `json:"content_start_line_number"`
	Content                string   `json:"content"`
	MatchedQueries         []string `json:"matched_queries"`
}
type MemorySearchResult struct {
	snapshot   uint64
	Queries    []string        `json:"queries"`
	MatchMode  MemoryMatchMode `json:"match_mode"`
	Path       *string         `json:"path"`
	Matches    []MemoryMatch   `json:"matches"`
	NextCursor *string         `json:"next_cursor"`
	Truncated  bool            `json:"truncated"`
}

func memoryPath(r NativeResource) string {
	switch r.Domain {
	case "memory.note":
		return notePrefix + r.Key
	case "memory.artifact":
		return r.Key
	case "memory.extension":
		return "extensions/" + r.Key
	case "memory.stage1":
		return "rollout_summaries/" + r.Key
	}
	return ""
}
func memoryScope(p *string) (string, error) {
	if p == nil || *p == "" || *p == "." {
		return "", nil
	}
	if !validNativePath(*p) {
		return "", errors.New("invalid_path")
	}
	return *p, nil
}
func hiddenMemory(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

// A snapshot owns bounded-per-resource content and is taken under the journal
// lock. The generation binds cursors to all commits, including legacy changes.
func (s *Store) memorySnapshot() (map[string]string, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned {
		return nil, 0, errors.New("state unavailable")
	}
	files := map[string]string{}
	for _, r := range s.native {
		p := memoryPath(r)
		if p == "" || r.Deleted {
			continue
		}
		if _, exists := files[p]; exists {
			return nil, 0, errors.New("ambiguous memory path")
		}
		if !utf8.Valid(r.Content) {
			continue
		}
		files[p] = string(r.Content)
	}
	return files, s.sequence, nil
}
func (s *Store) MemoryNote(actor NativeActor, callID, filename, note string) (NativeResult, error) {
	if actor.Kind != "model_tool" || actor.ThreadID == "" || !logicalID.MatchString(callID) {
		return NativeResult{}, errors.New("invalid trusted call identity")
	}
	if len(filename) > 128 || !noteFilename.MatchString(filename) {
		return NativeResult{}, errors.New("invalid_filename")
	}
	if strings.TrimSpace(note) == "" || !utf8.ValidString(note) {
		return NativeResult{}, errors.New("empty or invalid note")
	}
	actor.CallID = callID
	q := NativeRequest{RequestID: "note-" + hashBytes([]byte(actor.ThreadID+"\x00"+callID)), Actor: actor, Operation: "memory.note.create", Changes: []NativeChange{{Domain: "memory.note", Key: filename, Content: []byte(note)}}}
	return s.NativeBatch(q)
}

type memoryCursor struct {
	Generation uint64 `json:"generation"`
	Query      string `json:"query"`
	Index      int    `json:"index"`
}

func cursorStart(cursor *string, generation uint64, q any) (int, error) {
	if cursor == nil {
		return 0, nil
	}
	if len(*cursor) > 1024 {
		return 0, errors.New("invalid_cursor")
	}
	b, e := base64.RawURLEncoding.DecodeString(*cursor)
	if e != nil {
		return 0, errors.New("invalid_cursor")
	}
	var c memoryCursor
	if strictJSON(b, &c) != nil || c.Index < 0 {
		return 0, errors.New("invalid_cursor")
	}
	qb, _ := json.Marshal(q)
	if c.Generation != generation || c.Query != hashBytes(qb) {
		return 0, errors.New("snapshot_expired")
	}
	return c.Index, nil
}
func nextMemoryCursor(index int, generation uint64, q any) *string {
	qb, _ := json.Marshal(q)
	b, _ := json.Marshal(memoryCursor{Generation: generation, Query: hashBytes(qb), Index: index})
	out := base64.RawURLEncoding.EncodeToString(b)
	return &out
}
func (s *Store) MemoryList(q MemoryListRequest) (MemoryListResult, error) {
	out := MemoryListResult{Path: q.Path, Entries: []MemoryEntry{}}
	scope, e := memoryScope(q.Path)
	if e != nil {
		return out, e
	}
	if q.MaxResults <= 0 {
		return out, errors.New("invalid max_results")
	}
	q.MaxResults = min(q.MaxResults, 2000)
	files, gen, e := s.memorySnapshot()
	if e != nil {
		return out, e
	}
	out.snapshot = gen
	query := q
	query.Cursor = nil
	start, e := cursorStart(q.Cursor, gen, query)
	if e != nil {
		return out, e
	}
	entries := map[string]string{}
	if _, ok := files[scope]; ok {
		entries[scope] = "file"
	} else {
		prefix := scope
		if prefix != "" {
			prefix += "/"
		}
		for p := range files {
			if !strings.HasPrefix(p, prefix) || hiddenMemory(p) {
				continue
			}
			rest := strings.TrimPrefix(p, prefix)
			name, _, dir := strings.Cut(rest, "/")
			typ := "file"
			if dir {
				typ = "directory"
			}
			entries[prefix+name] = typ
		}
		if scope != "" && len(entries) == 0 {
			return out, errors.New("not_found")
		}
	}
	paths := make([]string, 0, len(entries))
	for p := range entries {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	if start > len(paths) {
		return out, errors.New("invalid_cursor")
	}
	size := 0
	i := start
	for ; i < len(paths) && len(out.Entries) < q.MaxResults; i++ {
		entry := MemoryEntry{Path: paths[i], EntryType: entries[paths[i]]}
		size += len(entry.Path)
		if size > NativePageBytes {
			break
		}
		out.Entries = append(out.Entries, entry)
	}
	if i < len(paths) {
		out.NextCursor = nextMemoryCursor(i, gen, query)
		out.Truncated = true
	}
	return out, nil
}
func bytePrefix(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}
func (s *Store) MemoryRead(q MemoryReadRequest) (MemoryReadResult, error) {
	out := MemoryReadResult{Path: q.Path, StartLineNumber: q.LineOffset}
	if !validNativePath(q.Path) {
		return out, errors.New("invalid_path")
	}
	if q.LineOffset < 1 {
		return out, errors.New("invalid_line_offset")
	}
	if q.MaxLines != nil && *q.MaxLines < 1 {
		return out, errors.New("invalid_max_lines")
	}
	files, gen, e := s.memorySnapshot()
	if e != nil {
		return out, e
	}
	out.snapshot = gen
	if q.ExpectedSequence != nil && *q.ExpectedSequence != gen {
		return out, errors.New("snapshot_expired")
	}
	content, ok := files[q.Path]
	if !ok {
		return out, errors.New("not_found")
	}
	start := 0
	for line := 1; line < q.LineOffset; line++ {
		n := strings.IndexByte(content[start:], '\n')
		if n < 0 {
			return out, errors.New("line_offset_exceeds_file_length")
		}
		start += n + 1
	}
	end := len(content)
	if q.MaxLines != nil {
		pos := start
		for line := 0; line < *q.MaxLines; line++ {
			n := strings.IndexByte(content[pos:], '\n')
			if n < 0 {
				pos = len(content)
				break
			}
			pos += n + 1
		}
		end = pos
	}
	out.Content = bytePrefix(content[start:end], NativePageBytes)
	out.Truncated = end < len(content) || len(out.Content) < end-start
	return out, nil
}
func prepareMemory(s string, caseSensitive, normalized bool) string {
	if !caseSensitive {
		s = strings.ToLower(s)
	}
	if normalized {
		s = strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || unicode.IsNumber(r) {
				return r
			}
			return -1
		}, s)
	}
	return s
}
func (s *Store) MemorySearch(q MemorySearchRequest) (MemorySearchResult, error) {
	out := MemorySearchResult{Queries: append([]string(nil), q.Queries...), MatchMode: q.MatchMode, Path: q.Path, Matches: []MemoryMatch{}}
	scope, e := memoryScope(q.Path)
	if e != nil {
		return out, e
	}
	if len(q.Queries) == 0 || len(q.Queries) > 64 || q.ContextLines < 0 || q.ContextLines > 1000 || q.MaxResults < 1 {
		return out, errors.New("invalid search bounds")
	}
	switch q.MatchMode.Type {
	case "any", "all_on_same_line":
		if q.MatchMode.LineCount != 0 {
			return out, errors.New("invalid_match_window")
		}
	case "all_within_lines":
		if q.MatchMode.LineCount < 1 || q.MatchMode.LineCount > 1000 {
			return out, errors.New("invalid_match_window")
		}
	default:
		return out, errors.New("invalid_match_mode")
	}
	prepared := []string{}
	for i, query := range q.Queries {
		if len(query) > 1024 {
			return out, errors.New("query too large")
		}
		out.Queries[i] = strings.TrimSpace(query)
		p := prepareMemory(out.Queries[i], q.CaseSensitive, q.Normalized)
		if p == "" {
			return out, errors.New("empty_query")
		}
		prepared = append(prepared, p)
	}
	q.MaxResults = min(q.MaxResults, 200)
	files, gen, e := s.memorySnapshot()
	if e != nil {
		return out, e
	}
	out.snapshot = gen
	query := q
	query.Cursor = nil
	start, e := cursorStart(q.Cursor, gen, query)
	if e != nil {
		return out, e
	}
	paths := []string{}
	for p := range files {
		if (scope == "" || p == scope || strings.HasPrefix(p, scope+"/")) && !hiddenMemory(p) {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	if scope != "" && len(paths) == 0 {
		return out, errors.New("not_found")
	}
	// Stream matches into a single bounded page; a cursor indexes the stable
	// logical match sequence, so we never retain every match's duplicated body.
	index, size := 0, 0
	for _, p := range paths {
		content := files[p]
		lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
		if content == "" {
			lines = nil
		}
		for i := range lines {
			lines[i] = strings.TrimSuffix(lines[i], "\r")
		}
		flags := make([]uint64, len(lines))
		for i, line := range lines {
			line = prepareMemory(line, q.CaseSensitive, q.Normalized)
			for j, term := range prepared {
				if strings.Contains(line, term) {
					flags[i] |= uint64(1) << j
				}
			}
		}
		all := ^uint64(0)
		if len(prepared) < 64 {
			all = (uint64(1) << len(prepared)) - 1
		}
		type window struct {
			a, b  int
			flags uint64
		}
		windows := []window{}
		// Next occurrence per query finds shortest windows in O(lines*queries),
		// without scanning up to 1000 lines again for every start position.
		ends := make([]int, len(lines))
		next := make([]int, len(prepared))
		for j := range next {
			next[j] = len(lines)
		}
		for i := len(lines) - 1; i >= 0; i-- {
			end := i
			for j := range next {
				if flags[i]&(uint64(1)<<j) != 0 {
					next[j] = i
				}
				end = max(end, next[j])
			}
			ends[i] = end
		}
		for i, f := range flags {
			if f == 0 {
				continue
			}
			end, combined := i, f
			if q.MatchMode.Type == "all_within_lines" {
				end = ends[i]
				if end >= len(lines) || end-i+1 > q.MatchMode.LineCount {
					continue
				}
				combined = all
			}
			if q.MatchMode.Type != "any" && combined != all {
				continue
			}
			windows = append(windows, window{i, end, combined})
		}

		for wi, w := range windows {
			// End indices are nondecreasing for these shortest windows. An adjacent
			// contained window therefore suffices to reject any nonminimal window.
			if q.MatchMode.Type == "all_within_lines" && wi+1 < len(windows) && windows[wi+1].b <= w.b {
				continue
			}
			if index < start {
				index++
				continue
			}
			if len(out.Matches) >= q.MaxResults {
				out.NextCursor = nextMemoryCursor(index, gen, query)
				out.Truncated = true
				return out, nil
			}
			a := max(0, w.a-q.ContextLines)
			b := min(len(lines), w.b+q.ContextLines+1)
			body := strings.Join(lines[a:b], "\n")
			if len(body) > NativePageBytes {
				return out, errors.New("search_match_too_large; reduce context or read the file")
			}
			if size+len(body) > NativePageBytes {
				out.NextCursor = nextMemoryCursor(index, gen, query)
				out.Truncated = true
				return out, nil
			}
			matched := []string{}
			for j, term := range out.Queries {
				if w.flags&(uint64(1)<<j) != 0 {
					matched = append(matched, term)
				}
			}
			out.Matches = append(out.Matches, MemoryMatch{Path: p, MatchLineNumber: w.a + 1, ContentStartLineNumber: a + 1, Content: body, MatchedQueries: matched})
			size += len(body)
			index++
		}
	}
	if start > index {
		return out, fmt.Errorf("invalid_cursor")
	}
	return out, nil
}

// Different canonical domains must not publish the same file or turn an
// existing file into a parent directory in the merged native memory view.
func (s *Store) validMemoryPaths(q NativeRequest) bool {
	changes := map[string]bool{}
	for _, c := range q.Changes {
		changes[nativeKey(c.Domain, c.Key)] = true
	}
	paths := map[string]bool{}
	add := func(r NativeResource) bool {
		p := memoryPath(r)
		if p == "" || r.Deleted {
			return true
		}
		if paths[p] {
			return false
		}
		paths[p] = true
		return true
	}
	for k, r := range s.native {
		if !changes[k] && !add(r) {
			return false
		}
	}
	for _, c := range q.Changes {
		if !add(NativeResource{Domain: c.Domain, Key: c.Key, Deleted: c.Deleted}) {
			return false
		}
	}
	for p := range paths {
		for {
			i := strings.LastIndexByte(p, '/')
			if i < 0 {
				break
			}
			p = p[:i]
			if paths[p] {
				return false
			}
		}
	}
	return true
}
