package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// MemoryEntry is a single stored memory.
type MemoryEntry struct {
	ID    string   `json:"id"`
	Scope string   `json:"scope,omitempty"`
	Text  string   `json:"text"`
	Tags  []string `json:"tags,omitempty"`
	TS    string   `json:"ts"`
}

// MemoryConfig controls memory injection behavior.
type MemoryConfig struct {
	InjectMode     string `json:"inject_mode,omitempty"`      // "relevant"|"recent_only"|"off"
	MaxInjectChars int    `json:"max_inject_chars,omitempty"` // default 2000
	MaxInjectCount int    `json:"max_inject_count,omitempty"` // default 10
	MaxTotal       int    `json:"max_total,omitempty"`        // default 500
}

// MemoryStore manages persistent agent memories backed by a JSONL file.
type MemoryStore struct {
	mu      sync.Mutex
	dir     string
	entries []MemoryEntry
	nextID  int
	mtime   time.Time
	config  MemoryConfig
}

const memoriesFile = "memories.jsonl"

// NewMemoryStore creates a memory store and loads existing entries.
func NewMemoryStore(dir string, config MemoryConfig) *MemoryStore {
	ms := &MemoryStore{dir: dir, config: config}
	ms.Load()
	return ms
}

// Load reads memories from the JSONL file.
func (ms *MemoryStore) Load() {
	path := filepath.Join(ms.dir, memoriesFile)
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	info, _ := f.Stat()
	if info != nil {
		ms.mtime = info.ModTime()
	}

	ms.entries = nil
	ms.nextID = 0

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	for scanner.Scan() {
		var entry MemoryEntry
		if json.Unmarshal(scanner.Bytes(), &entry) == nil && entry.ID != "" {
			ms.entries = append(ms.entries, entry)
			if n := parseMemoryID(entry.ID); n >= ms.nextID {
				ms.nextID = n + 1
			}
		}
	}
}

// reloadIfChanged checks file mtime and reloads if the file was modified externally.
func (ms *MemoryStore) reloadIfChanged() {
	path := filepath.Join(ms.dir, memoriesFile)
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if info.ModTime().After(ms.mtime) {
		ms.Load()
	}
}

// Store appends a new memory. Returns the generated ID.
func (ms *MemoryStore) Store(text string, tags []string, scope string) (string, error) {
	if text == "" {
		return "", fmt.Errorf("text is required")
	}
	if len(text) > 500 {
		return "", fmt.Errorf("text exceeds 500 character limit (%d chars)", len(text))
	}
	if looksLikeSecret(text) {
		return "", fmt.Errorf("text appears to contain a secret — not stored")
	}
	if scope == "" {
		scope = "workspace"
	}

	ms.mu.Lock()
	defer ms.mu.Unlock()

	ms.reloadIfChanged()

	// Prune if at capacity
	maxTotal := ms.config.MaxTotal
	if maxTotal == 0 {
		maxTotal = 500
	}
	if len(ms.entries) >= maxTotal {
		excess := len(ms.entries) - maxTotal + 1
		ms.entries = ms.entries[excess:]
		ms.rewriteFile()
	}

	id := fmt.Sprintf("m-%d", ms.nextID)
	ms.nextID++

	entry := MemoryEntry{
		ID:    id,
		Scope: scope,
		Text:  text,
		Tags:  tags,
		TS:    time.Now().UTC().Format(time.RFC3339),
	}

	// Append to file
	os.MkdirAll(ms.dir, 0755)
	path := filepath.Join(ms.dir, memoriesFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return "", fmt.Errorf("open memory file: %w", err)
	}
	data, _ := json.Marshal(entry)
	f.Write(data)
	f.Write([]byte{'\n'})
	f.Sync()
	f.Close()

	ms.entries = append(ms.entries, entry)

	info, _ := os.Stat(path)
	if info != nil {
		ms.mtime = info.ModTime()
	}

	return id, nil
}

// Search finds memories matching a query and/or tag.
func (ms *MemoryStore) Search(query, tag string) []MemoryEntry {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	ms.reloadIfChanged()

	queryLower := strings.ToLower(query)
	tagLower := strings.ToLower(tag)

	var matches []MemoryEntry
	// Iterate newest first
	for i := len(ms.entries) - 1; i >= 0; i-- {
		e := ms.entries[i]

		if query != "" {
			if !strings.Contains(strings.ToLower(e.Text), queryLower) {
				continue
			}
		}

		if tag != "" {
			found := false
			for _, t := range e.Tags {
				if strings.EqualFold(t, tagLower) {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		matches = append(matches, e)
		if len(matches) >= 20 {
			break
		}
	}

	return matches
}

// Delete removes a memory by ID using atomic file rewrite.
func (ms *MemoryStore) Delete(id string) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	ms.reloadIfChanged()

	found := false
	var remaining []MemoryEntry
	for _, e := range ms.entries {
		if e.ID == id {
			found = true
			continue
		}
		remaining = append(remaining, e)
	}
	if !found {
		return fmt.Errorf("memory %s not found", id)
	}

	ms.entries = remaining
	return ms.rewriteFile()
}

// rewriteFile atomically rewrites the JSONL file from in-memory entries.
func (ms *MemoryStore) rewriteFile() error {
	os.MkdirAll(ms.dir, 0755)
	path := filepath.Join(ms.dir, memoriesFile)
	tmp := path + ".tmp"

	f, err := os.Create(tmp)
	if err != nil {
		return err
	}

	for _, e := range ms.entries {
		data, _ := json.Marshal(e)
		f.Write(data)
		f.Write([]byte{'\n'})
	}
	f.Sync()
	f.Close()

	if err := os.Rename(tmp, path); err != nil {
		return err
	}

	info, _ := os.Stat(path)
	if info != nil {
		ms.mtime = info.ModTime()
	}
	return nil
}

// InjectBlock builds the [Memories] block for system prompt injection.
func (ms *MemoryStore) InjectBlock(lastUserMessage string) string {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	ms.reloadIfChanged()

	if len(ms.entries) == 0 {
		return ""
	}

	mode := ms.config.InjectMode
	if mode == "" {
		mode = "relevant"
	}
	if mode == "off" {
		return ""
	}

	maxChars := ms.config.MaxInjectChars
	if maxChars == 0 {
		maxChars = 2000
	}
	maxCount := ms.config.MaxInjectCount
	if maxCount == 0 {
		maxCount = 10
	}

	var selected []MemoryEntry

	if mode == "relevant" && lastUserMessage != "" {
		userWords := tokenize(lastUserMessage)
		if len(userWords) > 0 {
			type scored struct {
				entry MemoryEntry
				score float64
			}
			var results []scored
			for _, e := range ms.entries {
				s := scoreRelevance(e, userWords)
				if s > 0 {
					results = append(results, scored{e, s})
				}
			}
			sort.Slice(results, func(i, j int) bool {
				return results[i].score > results[j].score
			})
			for _, r := range results {
				selected = append(selected, r.entry)
			}
		}
	}

	// Fallback: most recent 5
	if len(selected) == 0 {
		start := len(ms.entries) - 5
		if start < 0 {
			start = 0
		}
		selected = ms.entries[start:]
	}

	// Apply budget
	var result []MemoryEntry
	totalChars := 0
	for _, e := range selected {
		if len(result) >= maxCount {
			break
		}
		if totalChars+len(e.Text) > maxChars && len(result) > 0 {
			break
		}
		result = append(result, e)
		totalChars += len(e.Text)
	}

	if len(result) == 0 {
		return ""
	}

	var buf strings.Builder
	buf.WriteString("[Memories]\n")
	for _, e := range result {
		tag := ""
		if len(e.Tags) > 0 {
			tag = e.Tags[0]
		}
		if tag != "" {
			fmt.Fprintf(&buf, "- (%s, %s) %s\n", e.ID, tag, e.Text)
		} else {
			fmt.Fprintf(&buf, "- (%s) %s\n", e.ID, e.Text)
		}
	}
	return buf.String()
}

// scoreRelevance computes a relevance score for a memory against user message tokens.
func scoreRelevance(entry MemoryEntry, userWords map[string]bool) float64 {
	entryWords := tokenize(entry.Text)
	overlap := 0
	for w := range entryWords {
		if userWords[w] {
			overlap++
		}
	}

	if overlap == 0 {
		return 0
	}

	score := float64(overlap)

	// Recency bonus (only when there's keyword overlap)
	ts, err := time.Parse(time.RFC3339, entry.TS)
	if err == nil {
		ageHours := time.Since(ts).Hours()
		if ageHours < 0.1 {
			ageHours = 0.1
		}
		bonus := 0.1 / ageHours
		if bonus > 1.0 {
			bonus = 1.0
		}
		score += bonus
	}

	return score
}

// tokenize splits text into a set of lowercase words, dropping short words and stopwords.
func tokenize(text string) map[string]bool {
	words := make(map[string]bool)
	for _, word := range splitWords(text) {
		w := strings.ToLower(word)
		if len(w) < 3 {
			continue
		}
		if stopwords[w] {
			continue
		}
		words[w] = true
	}
	return words
}

// splitWords splits text on non-alphanumeric boundaries.
func splitWords(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// Secret detection patterns.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bsk-[a-zA-Z0-9\-]{20,}`),
	regexp.MustCompile(`(?i)\bghp_[a-zA-Z0-9]{30,}`),
	regexp.MustCompile(`(?i)\bgho_[a-zA-Z0-9]{30,}`),
	regexp.MustCompile(`(?i)\bglpat-[a-zA-Z0-9]{20,}`),
	regexp.MustCompile(`(?i)\bxox[bp]-[a-zA-Z0-9\-]{20,}`),
	regexp.MustCompile(`(?i)\bBearer\s+[a-zA-Z0-9._\-]{20,}`),
	regexp.MustCompile(`(?i)\btoken:\s*\S{20,}`),
	regexp.MustCompile(`(?i)\bpassword:\s*\S{8,}`),
}

var highEntropyPattern = regexp.MustCompile(`[a-zA-Z0-9+/]{40,}`)

// looksLikeSecret returns true if the text appears to contain a secret or API key.
func looksLikeSecret(text string) bool {
	for _, re := range secretPatterns {
		if re.MatchString(text) {
			return true
		}
	}
	if match := highEntropyPattern.FindString(text); match != "" {
		hasUpper, hasLower, hasDigit := false, false, false
		for _, c := range match {
			switch {
			case c >= 'A' && c <= 'Z':
				hasUpper = true
			case c >= 'a' && c <= 'z':
				hasLower = true
			case c >= '0' && c <= '9':
				hasDigit = true
			}
		}
		if hasUpper && hasLower && hasDigit {
			return true
		}
	}
	return false
}

// parseMemoryID extracts the numeric part from "m-<N>".
func parseMemoryID(id string) int {
	n := 0
	if strings.HasPrefix(id, "m-") {
		fmt.Sscanf(id[2:], "%d", &n)
	}
	return n
}

// stopwords is a set of high-frequency low-signal words excluded from relevance scoring.
var stopwords = func() map[string]bool {
	words := []string{
		"the", "and", "for", "are", "but", "not", "you", "all", "can", "has", "her",
		"was", "one", "our", "out", "its", "use", "how", "may", "who", "did", "get",
		"had", "him", "his", "let", "say", "she", "too", "own", "way", "about",
		"could", "from", "have", "into", "just", "like", "make", "many", "some",
		"than", "that", "them", "then", "this", "very", "when", "what", "with",
		"will", "would", "been", "each", "more", "most", "much", "must", "only",
		"also", "back", "being", "come", "every", "first", "here", "know", "made",
		"need", "over", "such", "take", "where", "which", "while", "work",
		"project", "please", "help", "want", "using", "thing", "file", "should",
	}
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}()
