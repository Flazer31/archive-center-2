// Package diagnostics owns the shareable, bounded diagnostic projection.
// It never reads token, DSN, encryption-key, chat, or memory files.
package diagnostics

import (
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// TailBytes is the most of one log file put in the report (its end).
const TailBytes = 4 << 20

var patterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(bearer\s+)[^\s"',;]+`),
	regexp.MustCompile(`(?i)((?:api[_ -]?key|access[_ -]?token|authorization|[a-z_]*password|passwd|secret|token)["']?\s*[:=]\s*["']?)[^\s"',;&}]+`),
	regexp.MustCompile(`(?i)(https?://)[^/\s:@]+:[^/\s@]+@`),
	regexp.MustCompile(`[^\s"':]+:[^\r\n]+@(?:tcp|unix)\([^)]*\)`),
	// a key starts a word: "task-queue-12345678" and "disk-usage-report" in a
	// path are not keys (user 11-10: only credentials are masked)
	regexp.MustCompile(`(^|[^A-Za-z0-9])(?:ac2-(?:[a-z2-7]{6}-){3}[a-z2-7]{6}|sk-[A-Za-z0-9_-]{8,}|AIza[A-Za-z0-9_-]{20,})`),
	regexp.MustCompile(`(?i)(IDENTIFIED\s+BY\s+['"])[^'"]*`),
}

// narrowDSN is a DSN whose password has no space or "@": only the password
// is masked, so the error around it (user, address, cause) stays readable
// (review 22 B-2). A DSN the narrow form does not cover falls to the broad
// pattern above (a password may hold spaces, colons and @).
var narrowDSN = regexp.MustCompile(`(^|[\s"'=(])([^\s"':@]+):[^\s@]+@((?:tcp|unix)\([^)]*\))`)

var quotedCredential = regexp.MustCompile(`(?i)((?:api[_ -]?key|access[_ -]?token|authorization|[a-z_]*password|passwd|secret|token)["']?\s*[:=]\s*)(?:"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*')`)

var logRecordStart = regexp.MustCompile(`^\d{4}[/-]\d{2}[/-]\d{2}[ T]\s*\d{1,2}:\d{2}:\d{2}`)

// ErrorSummary is the error as it was, for the user to read in the HUD,
// status, logs and the diagnostic report (user 11-08: "이렇게 막혀있는
// 것들은 다 풀어서 볼 수 있게 해줘"). Only credential values (API keys,
// tokens, passwords, DSN credentials) are masked; very long text is cut at
// ErrorLimit characters.
func ErrorSummary(text string) string {
	text = Redact(strings.TrimSpace(text))
	if r := []rune(text); len(r) > ErrorLimit {
		text = string(r[:ErrorLimit]) + "…"
	}
	return text
}

// ErrorLimit is the most of one error kept for reading.
const ErrorLimit = 8000

// IndentedLog keeps one log record together: lines after the first are
// indented, so the report reader treats them as that record's continuation.
func IndentedLog(w io.Writer) io.Writer { return indented{w} }

type indented struct{ w io.Writer }

func (x indented) Write(p []byte) (int, error) {
	body := strings.TrimRight(string(p), "\r\n")
	body = strings.NewReplacer("\r\n", "\n    ", "\n", "\n    ", "\r", "\n    ").Replace(body)
	if _, err := io.WriteString(x.w, body+"\n"); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Redact is shared by startup logging, managed MariaDB, and both exporters.
// Known in-memory secrets also cover arbitrary credential formats; no secret
// files are opened merely to produce a report.
func Redact(text string, secrets ...string) string {
	for _, value := range secrets {
		if value != "" {
			text = strings.ReplaceAll(text, value, "[가림]")
			encoded, _ := json.Marshal(value)
			text = strings.ReplaceAll(text, string(encoded[1:len(encoded)-1]), "[가림]")
			// as it appears in an address (review 22 A-3)
			for _, e := range []string{url.QueryEscape(value), url.PathEscape(value)} {
				if e != value {
					text = strings.ReplaceAll(text, e, "[가림]")
				}
			}
		}
	}
	text = narrowDSN.ReplaceAllString(text, "${1}${2}:[가림]@${3}")
	text = quotedCredential.ReplaceAllString(text, "${1}[가림]")
	for i, pattern := range patterns {
		replacement := "${1}[가림]"
		if i == 3 {
			replacement = "[접속 정보 가림]"
		}
		if i == 4 {
			replacement = "${1}[접속 정보 가림]"
		}
		if i == 3 {
			// Timestamps contain colons too. Keep their anchored prefix out
			// of the DSN match; passwords themselves may contain spaces,
			// colons and @, so those cannot be delimiter assumptions.
			lines := strings.SplitAfter(text, "\n")
			for n, line := range lines {
				at := 0
				if prefix := logRecordStart.FindStringIndex(line); prefix != nil {
					at = prefix[1]
				}
				// a DSN already masked by narrowDSN stays as it is
				if !strings.Contains(strings.ReplaceAll(strings.ReplaceAll(line[at:], ":[가림]@tcp(", ""), ":[가림]@unix(", ""), "@tcp(") &&
					!strings.Contains(strings.ReplaceAll(line[at:], ":[가림]@unix(", ""), "@unix(") {
					continue
				}
				lines[n] = line[:at] + pattern.ReplaceAllString(line[at:], replacement)
			}
			text = strings.Join(lines, "")
		} else {
			text = pattern.ReplaceAllString(text, replacement)
		}
	}
	return strings.ToValidUTF8(text, "�")
}

// ShortPath keeps paths as they are: full paths help find a fault (user
// 11-08 asked for nothing to be hidden but credentials).
func ShortPath(text string) string { return text }

// Clean recursively scrubs every string of a JSON-shaped diagnostic projection.
// Callers must first select allowed fields; scrubbing is not content selection.
func Clean(value any, secrets ...string) any {
	switch v := value.(type) {
	case string:
		return ShortPath(Redact(v, secrets...))
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[ShortPath(Redact(k, secrets...))] = Clean(x, secrets...)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = Clean(x, secrets...)
		}
		return out
	default:
		return value
	}
}

type LogFile struct {
	Name      string `json:"name"`
	Text      string `json:"text,omitempty"`
	Error     string `json:"error,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type Report struct {
	Contract      string     `json:"contract_version"`
	GeneratedAt   time.Time  `json:"generated_at"`
	Version       string     `json:"version"`
	OS            string     `json:"os"`
	Arch          string     `json:"arch"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	UptimeSeconds int64      `json:"uptime_seconds,omitempty"`
	DataDirectory string     `json:"data_directory"`
	Files         []LogFile  `json:"files"`
}

func NewReport(dataDir, version string, startedAt time.Time) Report {
	now := time.Now().UTC()
	r := Report{Contract: "ac2.diagnostics.v1", GeneratedAt: now, Version: Redact(version), OS: runtime.GOOS, Arch: runtime.GOARCH, DataDirectory: ShortPath(Redact(dataDir)), Files: CollectLogs(dataDir)}
	if !startedAt.IsZero() {
		start := startedAt.UTC()
		r.StartedAt, r.UptimeSeconds = &start, max(0, int64(now.Sub(start).Seconds()))
	}
	return r
}

// CollectLogs opens the newest LogFiles start-*.log files (newest first)
// and mariadb.log.
func CollectLogs(dataDir string) []LogFile {
	files := []LogFile{}
	type logEntry struct {
		name string
		at   time.Time
	}
	var starts []logEntry
	entries, err := os.ReadDir(filepath.Join(dataDir, "logs"))
	if err != nil {
		files = append(files, LogFile{Name: "startup", Error: Redact(err.Error())})
	} else {
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasPrefix(name, "start-") || !strings.HasSuffix(name, ".log") || !entry.Type().IsRegular() {
				continue
			}
			if info, err := entry.Info(); err == nil {
				starts = append(starts, logEntry{name, info.ModTime()})
			}
		}
		sort.Slice(starts, func(i, j int) bool {
			if !starts[i].at.Equal(starts[j].at) {
				return starts[i].at.After(starts[j].at)
			}
			return starts[i].name > starts[j].name
		})
		if len(starts) == 0 {
			files = append(files, LogFile{Name: "startup", Error: "시작 로그가 없습니다."})
		}
		for i, e := range starts {
			if i == LogFiles {
				break
			}
			files = append(files, readTail(filepath.Join(dataDir, "logs", e.name), "logs/"+e.name))
		}
	}
	return append(files, readTail(filepath.Join(dataDir, "mariadb.log"), "mariadb.log"))
}

// LogFiles is how many of the newest start logs the report holds.
const LogFiles = 3

func readTail(path, name string) LogFile {
	item := LogFile{Name: ShortPath(Redact(name))}
	info, err := os.Lstat(path)
	if err == nil && !info.Mode().IsRegular() {
		item.Error = "일반 로그 파일이 아닙니다."
		return item
	}
	var f *os.File
	if err == nil {
		f, err = os.Open(path)
	}
	if err == nil {
		defer f.Close()
		if info.Size() > TailBytes {
			item.Truncated = true
			_, err = f.Seek(-TailBytes, io.SeekEnd)
		}
		if err == nil {
			var data []byte
			data, err = io.ReadAll(io.LimitReader(f, TailBytes))
			if item.Truncated {
				// Never export a credential cut halfway at the tail boundary.
				if at := strings.IndexByte(string(data), '\n'); at >= 0 {
					data = data[at+1:]
				} else {
					data = nil
				}
			}
			item.Text = Redact(string(data))
			if len(item.Text) > TailBytes {
				item.Truncated = true
				item.Text = item.Text[:TailBytes]
				for !utf8.ValidString(item.Text) {
					item.Text = item.Text[:len(item.Text)-1]
				}
			}
		}
	}
	if err != nil {
		item.Error = ShortPath(Redact(err.Error()))
	}
	return item
}
