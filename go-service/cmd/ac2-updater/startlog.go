package main

// The preserved runner is the single file-log owner for itself and its children.
// Scripts append only their own bootstrap messages, before this binary can run.
import (
	"bytes"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"archivecenter2/goservice/internal/diagnostics"
)

type startupLog struct {
	mu       sync.Mutex
	file     io.WriteCloser
	stage    string
	ready    bool
	out, err *startupStream
}

type startupStream struct {
	owner        *startupLog
	screen       io.Writer
	pending      []byte
	privateToken bool
}

func openStartupLog(name string, stdout, stderr io.Writer) (*startupLog, error) {
	f, err := os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	b := &startupLog{file: f, stage: "업데이트 도구 준비"}
	b.out = &startupStream{owner: b, screen: stdout}
	b.err = &startupStream{owner: b, screen: stderr}
	return b, nil
}

func (s *startupStream) Write(p []byte) (int, error) {
	b := s.owner
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := s.screen.Write(p)
	s.pending = append(s.pending, p...)
	for {
		i := bytes.IndexByte(s.pending, '\n')
		if i < 0 {
			break
		}
		s.record(string(s.pending[:i+1]))
		s.pending = s.pending[i+1:]
	}
	return n, err
}

var tokenRecord = regexp.MustCompile(`^(?:\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} )?새 접속 토큰이 만들어졌습니다\.`)
var tokenFileRecord = regexp.MustCompile(`^(?:\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} )?access token: .* \(ends with `)

func (s *startupStream) record(line string) {
	b := s.owner
	// only the backend's own token records (a record of their own, not text
	// quoted inside an error) hide what follows
	if tokenRecord.MatchString(line) {
		s.privateToken = true
		fmt.Fprintln(b.file, "접속 토큰은 화면에만 표시하며 시작 로그에는 기록하지 않습니다.")
		return
	}
	if tokenFileRecord.MatchString(line) {
		fmt.Fprintln(b.file, "접속 토큰 정보는 화면에만 표시하며 시작 로그에는 기록하지 않습니다.")
		return
	}
	if s.privateToken {
		if strings.HasPrefix(strings.TrimSpace(line), "(파일:") {
			s.privateToken = false
		} else {
			return
		}
	}
	// an indented line continues the record above it (diagnostics.IndentedLog);
	// text quoted inside an error is never a startup stage
	if !strings.HasPrefix(line, " ") {
		if i := strings.Index(line, "[시작] "); i >= 0 {
			b.stage = strings.TrimSpace(line[i+len("[시작] "):])
			b.ready = false
		} else if i := strings.Index(line, "/5]"); i >= 0 {
			b.stage = strings.TrimSpace(line[i+len("/5]"):])
		}
		if strings.Contains(line, "[준비 완료]") {
			b.ready = true
		}
	}
	line = diagnostics.Redact(line)
	if _, err := io.WriteString(b.file, line); err != nil {
		// A full disk must not terminate an otherwise healthy backend.
		fmt.Fprintln(s.screen, "시작 로그를 쓰지 못했습니다:", err)
	}
}

func (b *startupLog) heartbeat() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ready {
		return
	}
	line := fmt.Sprintf("아직 진행 중입니다: %s (백신 검사 등으로 늦어질 수 있어요).\n", b.stage)
	fmt.Fprint(b.err.screen, line)
	fmt.Fprint(b.file, line)
}

func (b *startupLog) progress() func() {
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				b.heartbeat()
			case <-done:
				return
			}
		}
	}()
	return func() { close(done); <-stopped }
}

func (b *startupLog) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range []*startupStream{b.out, b.err} {
		if len(s.pending) > 0 {
			s.record(string(s.pending))
			s.pending = nil
		}
	}
	b.file.Close()
}

func (b *startupLog) failure(err error) {
	b.mu.Lock()
	stage := b.stage
	b.mu.Unlock()
	code := exitCode(err)
	if code < 0 {
		code = 1
	}
	fmt.Fprintf(b.err, "오류: %s에서 멈췄습니다. 종료 코드: %d\n", stage, code)
	fmt.Fprintln(b.err, "실행 파일이 없거나 열리지 않으면 압축을 모두 풀었는지, 백신 차단·격리를 확인해 주세요. 접근 거부는 파일을 사용하는 프로그램을 확인한 뒤 다시 실행해 주세요.")
	fmt.Fprintln(b.err, "포트 사용 중이면 이미 켜진 시작 창을 확인하세요. MariaDB 내려받기 오류는 인터넷 연결과 디스크 공간을 확인해 주세요.")
	fmt.Fprintln(b.err, "시작 로그:", os.Getenv("AC2_START_LOG"))
}
