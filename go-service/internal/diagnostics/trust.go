package diagnostics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Windows trust diagnostics (1.0 O10, AC1 check-windows-trust.ps1): what a
// virus scanner or SmartScreen may see in the package — each program file's
// size and SHA256 (to send with a false-positive report), whether it still
// carries the downloaded-from-the-internet mark (Zone.Identifier), and the
// Microsoft Defender detections that name a file of this package or its data.
// Read only: nothing is changed, unblocked, excluded or turned off.

// TrustFile is one program file of the package.
type TrustFile struct {
	Path            string `json:"path"`
	Size            int64  `json:"size_bytes"`
	SHA256          string `json:"sha256"`
	MarkOfTheWeb    bool   `json:"has_mark_of_the_web"`
	InternetZoneTag string `json:"zone,omitempty"` // the mark's ZoneId (3 = internet)
}

// TrustReport is the package's trust picture.
type TrustReport struct {
	Note            string            `json:"note"`
	Files           []TrustFile       `json:"files"`
	MarkedFiles     int               `json:"mark_of_the_web_count"`
	Detections      []json.RawMessage `json:"defender_detections"`
	DetectionsError string            `json:"defender_detections_error,omitempty"`
	Hint            string            `json:"hint,omitempty"`
}

var trustExts = map[string]bool{".exe": true, ".dll": true, ".ps1": true, ".cmd": true, ".bat": true, ".sh": true}

// PackageTrust reads the package under root (the data folder is skipped:
// it holds no program of the package) and Defender's detections naming root
// or data.
func PackageTrust(root, data string) *TrustReport {
	r := &TrustReport{Note: "읽기 전용 진단입니다. Defender를 끄거나 예외를 추가하거나 파일을 바꾸지 않습니다.", Files: []TrustFile{}, Detections: []json.RawMessage{}}
	skip, _ := filepath.Abs(data)
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if abs, _ := filepath.Abs(path); skip != "" && strings.EqualFold(abs, skip) {
				return filepath.SkipDir
			}
			return nil
		}
		if !trustExts[strings.ToLower(filepath.Ext(path))] || !d.Type().IsRegular() {
			return nil
		}
		f := TrustFile{Path: filepath.ToSlash(rel(root, path))}
		if info, err := d.Info(); err == nil {
			f.Size = info.Size()
		}
		if h, err := fileSHA256(path); err == nil {
			f.SHA256 = h
		}
		f.MarkOfTheWeb, f.InternetZoneTag = zoneMark(path)
		if f.MarkOfTheWeb {
			r.MarkedFiles++
		}
		r.Files = append(r.Files, f)
		return nil
	})
	if r.MarkedFiles > 0 {
		r.Hint = "인터넷에서 받은 표시가 남은 파일이 있어요. Windows가 이 파일을 막거나 검사하느라 시작이 오래 걸릴 수 있어요. ZIP 파일을 풀기 전에 ZIP 파일 속성에서 '차단 해제'를 선택한 뒤 다시 풀면 표시가 사라져요."
	}
	if runtime.GOOS == "windows" {
		r.Detections, r.DetectionsError = defenderDetections(root, data)
	}
	return r
}

func rel(root, path string) string {
	if r, err := filepath.Rel(root, path); err == nil {
		return r
	}
	return path
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return strings.ToUpper(hex.EncodeToString(h.Sum(nil))), nil
}

// zoneMark reads the Zone.Identifier stream Windows adds to downloaded
// files (NTFS alternate data stream; absent elsewhere).
func zoneMark(path string) (bool, string) {
	if runtime.GOOS != "windows" {
		return false, ""
	}
	f, err := os.Open(path + ":Zone.Identifier")
	if err != nil {
		return false, ""
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, 4096))
	zone := ""
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ZoneId="); ok {
			zone = v
		}
	}
	return true, zone
}

// defenderDetections asks PowerShell for Defender's detection history (read
// only) and keeps the ones naming root or data.
func defenderDetections(root, data string) ([]json.RawMessage, string) {
	out := []json.RawMessage{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	script := `$t=@{}; try { Get-MpThreat -ErrorAction Stop | ForEach-Object { $t[[string]$_.ThreatID]=$_.ThreatName } } catch {}
@(Get-MpThreatDetection -ErrorAction Stop | ForEach-Object { [ordered]@{ time=$_.InitialDetectionTime.ToString('o'); threat_id=[string]$_.ThreatID; threat_name=$t[[string]$_.ThreatID]; action_success=$_.ActionSuccess; resources=@($_.Resources) } }) | ConvertTo-Json -Depth 4 -Compress`
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	b, err := cmd.Output()
	if err != nil {
		return out, Redact(err.Error())
	}
	text := strings.TrimSpace(string(b))
	if text == "" {
		return out, ""
	}
	var all []json.RawMessage
	if strings.HasPrefix(text, "{") {
		all = []json.RawMessage{json.RawMessage(text)}
	} else if err := json.Unmarshal([]byte(text), &all); err != nil {
		return out, "detections_not_read"
	}
	needles := []string{strings.ToLower(root), strings.ToLower(data), "archivecenter2", "ac2.exe", "ac2-updater"}
	for _, d := range all {
		low := strings.ToLower(strings.ReplaceAll(string(d), `\\`, `\`))
		for _, n := range needles {
			if n != "" && strings.Contains(low, n) {
				out = append(out, d)
				break
			}
		}
	}
	return out, ""
}
