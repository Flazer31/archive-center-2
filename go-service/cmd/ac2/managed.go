package main

// The package start file uses this launcher. It owns a separate MariaDB
// instance and invokes the same backend, with no credentials in argv.
import (
	"archive/zip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"archivecenter2/goservice/internal/diagnostics"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/osfile"
	"archivecenter2/goservice/internal/secret"
	"archivecenter2/goservice/internal/serve"
	"archivecenter2/goservice/internal/store"
	"github.com/go-sql-driver/mysql"
)

// Release identity and digest come from the official MariaDB archive's
// sha256sums.txt. This is a dependency pin, not memory-selection policy.
const mariaWindowsURL = "https://archive.mariadb.org/mariadb-13.0.2/winx64-packages/mariadb-13.0.2-winx64.zip"
const mariaWindowsSHA = "ba9cefd6674b3f2f7bc67334c05305dc7c919643d76836e10767e70db0b5fb74"

type installConfig struct {
	BackendPort  int    `json:"backend_port"`
	DatabasePort int    `json:"database_port"`
	RootPassword string `json:"root_password"`
	AppPassword  string `json:"app_password"`
}

func freePort(preferred int) (int, error) {
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(preferred)))
	if err != nil && preferred != 0 {
		l, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func installSettings(data string, port int) (installConfig, error) {
	name := filepath.Join(data, "install.json")
	var c installConfig
	b, err := os.ReadFile(name)
	if err == nil {
		if err = json.Unmarshal(b, &c); err != nil {
			return c, err
		}
		if c.BackendPort == port {
			return c, nil
		}
		c.BackendPort = port
		b, err = json.MarshalIndent(c, "", "  ")
		if err == nil {
			err = secret.WriteFile(name, b)
		}
		return c, err
	}
	if !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	c.BackendPort = port
	if c.DatabasePort, err = freePort(0); err != nil {
		return c, err
	}
	for _, dst := range []*string{&c.RootPassword, &c.AppPassword} {
		b := make([]byte, 32)
		if _, err = rand.Read(b); err != nil {
			return c, err
		}
		*dst = hex.EncodeToString(b)
	}
	b, err = json.MarshalIndent(c, "", "  ")
	if err == nil {
		err = secret.WriteFile(name, b)
	}
	return c, err
}

func (c installConfig) connection(user, password, database string) string {
	v := mysql.NewConfig()
	v.User, v.Passwd, v.DBName = user, password, database
	v.Net, v.Addr = "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(c.DatabasePort))
	v.ParseTime = true
	v.Timeout, v.ReadTimeout, v.WriteTimeout = 5*time.Second, 30*time.Second, 30*time.Second
	return v.FormatDSN()
}

func runManaged(dir string, port int, listen, cert, key, updateRepo, updateAPI string, updateAware bool) (result error) {
	stage := "백엔드 포트 확인"
	log.Printf("[시작] 백엔드 포트 %d 확인", port)
	defer func() {
		if result != nil {
			log.Printf("오류: %s에서 멈췄습니다.", stage)
		}
	}()
	if err := (serve.Config{Listen: listen, Port: port}).CheckAvailable(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	root := filepath.Dir(exe)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	data, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	stage = "데이터 폴더·설치 잠금 준비"
	log.Print("[시작] 데이터 폴더·설치 잠금 준비")
	if err = privateDirectory(data); err != nil {
		return err
	}
	unlock, err := lockInstall(filepath.Join(data, "run.lock"))
	if err != nil {
		return fmt.Errorf("이 설치의 시작 창이 이미 열려 있거나 데이터 폴더를 사용할 수 없습니다: %w", err)
	}
	defer unlock()
	cleanupJob, err := ownChildren()
	if err != nil {
		return err
	}
	defer cleanupJob()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stage = "설치 설정 준비"
	log.Print("[시작] 설치 설정 준비")
	c, err := installSettings(data, port)
	if err != nil {
		return fmt.Errorf("설치 설정을 준비하지 못했습니다: %w", err)
	}
	var daemon, initTool, base string
	stage = "MariaDB 준비"
	if runtime.GOOS == "windows" {
		base, err = windowsMariaDB(ctx)
		daemon, initTool = filepath.Join(base, "bin", "mariadbd.exe"), filepath.Join(base, "bin", "mariadb-install-db.exe")
	} else {
		log.Print("[1/5] 시스템에 설치된 MariaDB 실행 파일을 확인합니다.")
		daemon, err = exec.LookPath("mariadbd")
		if err == nil {
			initTool, err = exec.LookPath("mariadb-install-db")
		}
		if err == nil {
			// the backend needs MariaDB store.MinimumMariaDB or newer; an older system server
			// is told here, before it is started
			if v := systemMariaDBVersion(ctx, daemon); v != "" && store.CheckMariaDBVersion(v) != nil {
				return errors.New(mariaDBVersionHelp(v))
			}
			log.Print("[2/5] 시스템 MariaDB를 사용합니다. 별도 파일 내려받기·해시 확인은 필요하지 않습니다.")
			log.Print("[3/5] 시스템 MariaDB 준비 완료. 별도 압축 풀기는 필요하지 않습니다.")
		}
	}
	if err != nil {
		return fmt.Errorf("MariaDB를 준비하지 못했습니다. 시작 창의 오류와 인터넷 연결을 확인해 주세요: %w", err)
	}
	log.Print("[4/5] DB 초기화·계정·스키마 준비를 시작합니다.")
	stage = "DB 초기화·계정·스키마 준비"
	dbData := filepath.Join(data, "mariadb")
	if err = initializeMariaDB(ctx, dbData, initTool, c); err != nil {
		return err
	}
	// init-file runs before clients are accepted. Credentials never appear
	// in a command line; both this file and the persisted config are private.
	initSQL := filepath.Join(data, "db-init.sql")
	sqlText := "CREATE DATABASE IF NOT EXISTS ac2 CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;\n" +
		"CREATE USER IF NOT EXISTS 'ac2'@'127.0.0.1' IDENTIFIED BY '" + c.AppPassword + "';\n" +
		"ALTER USER 'ac2'@'127.0.0.1' IDENTIFIED BY '" + c.AppPassword + "';\n" +
		"GRANT ALL PRIVILEGES ON ac2.* TO 'ac2'@'127.0.0.1';\n" +
		"ALTER USER 'root'@'localhost' IDENTIFIED BY '" + c.RootPassword + "';\n" +
		"DELETE FROM mysql.global_priv WHERE User='' OR (User='root' AND Host<>'localhost');\n" +
		"FLUSH PRIVILEGES;\n"
	if err = secret.WriteFile(initSQL, []byte(sqlText)); err != nil {
		return err
	}
	defer os.Remove(initSQL)
	args := []string{"--no-defaults", "--datadir=" + dbData, "--bind-address=127.0.0.1", "--port=" + strconv.Itoa(c.DatabasePort), "--skip-log-bin", "--innodb-buffer-pool-size=64M", "--max-connections=80", "--init-file=" + initSQL}
	if base != "" {
		args = append(args, "--basedir="+base, "--console")
	}
	var socket string
	if runtime.GOOS != "windows" {
		// UNIX socket paths have a small OS limit; the private TCP listener
		// remains the application's connection on every platform.
		h := sha256.Sum256([]byte(data))
		socket = filepath.Join(os.TempDir(), "ac2-"+hex.EncodeToString(h[:12])+".sock")
		args = append(args, "--socket="+socket)
		defer os.Remove(socket)
	}
	logPath := filepath.Join(data, "mariadb.log")
	if err = secret.WriteFile(logPath, nil); err != nil {
		return err
	}
	dbLog, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer dbLog.Close()
	dbCmd := exec.Command(daemon, args...)
	hideChild(dbCmd)
	dbCmd.Stdout, dbCmd.Stderr = dbLog, dbLog
	if err = dbCmd.Start(); err != nil {
		return err
	}
	dbDone := make(chan error, 1)
	go func() { dbDone <- dbCmd.Wait() }()
	admin, err := sql.Open("mysql", c.connection("root", c.RootPassword, ""))
	if err != nil {
		dbCmd.Process.Kill()
		<-dbDone
		return err
	}
	defer admin.Close()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		admin.ExecContext(shutdown, "SHUTDOWN")
		select {
		case <-dbDone:
		case <-shutdown.Done():
			dbCmd.Process.Kill()
			<-dbDone
		}
		// A bootstrap error may quote an SQL statement in the server log.
		dbLog.Close()
		if b, e := os.ReadFile(logPath); e == nil {
			secret.WriteFile(logPath, []byte(scrubInstallLog(string(b), c)))
		}
	}()
	ready, cancelReady := context.WithTimeout(ctx, 3*time.Minute)
	defer cancelReady()
	for {
		if err = admin.PingContext(ready); err == nil {
			break
		}
		select {
		case e := <-dbDone:
			// Keep one completion for the shutdown owner above.
			dbDone <- e
			return fmt.Errorf("MariaDB가 시작되지 않았습니다. %s 로그를 확인해 주세요. 처음 실행이 중간에 끊겼다면 data 안 mariadb 폴더를 다른 이름으로 옮긴 뒤 다시 실행해 주세요", logPath)
		case <-ready.Done():
			return errors.New("MariaDB 연결 시간 초과. 시작 창과 data/mariadb.log를 확인해 주세요")
		case <-time.After(300 * time.Millisecond):
		}
	}
	if err = os.Remove(initSQL); err != nil {
		return err
	}
	if err = secret.WriteFile(filepath.Join(data, "mariadb.dsn"), []byte(c.connection("ac2", c.AppPassword, "ac2"))); err != nil {
		return err
	}
	childArgs := []string{"-data", data, "-port", strconv.Itoa(c.BackendPort), "-listen", listen}
	childArgs = append(childArgs, "-update-repo", updateRepo, "-update-api", updateAPI)
	if updateAware {
		childArgs = append(childArgs, "-update-managed")
	}
	if cert != "" {
		childArgs = append(childArgs, "-tls-cert", cert, "-tls-key", key)
	}
	child := exec.Command(exe, childArgs...)
	stage = "백엔드 연결 준비"
	log.Print("[시작] 백엔드 연결 준비")
	child.Stdout, child.Stderr, child.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err = child.Start(); err != nil {
		return err
	}
	pidPath := filepath.Join(data, "backend.pid")
	if err = secret.WriteFile(pidPath, []byte(strconv.Itoa(child.Process.Pid))); err != nil {
		child.Process.Kill()
		child.Wait()
		return err
	}
	defer os.Remove(pidPath)
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case e := <-done:
		return e
	case <-ctx.Done():
		child.Process.Kill()
		<-done
		return nil
	case e := <-dbDone:
		dbDone <- e
		child.Process.Kill()
		<-done
		return errors.New("MariaDB가 종료되었습니다. 시작 파일로 다시 실행해 주세요")
	}
}

// systemMariaDBVersion is the version "mariadbd --version" reports
// ("mariadbd  Ver 10.11.13-MariaDB-0ubuntu0.24.04.1 for ..." gives
// "10.11.13"); "" when it cannot be read (then the backend still checks).
func systemMariaDBVersion(ctx context.Context, daemon string) string {
	out, err := exec.CommandContext(ctx, daemon, "--no-defaults", "--version").Output() // as it is started
	if err != nil {
		return ""
	}
	return mariaDBVersionIn(string(out))
}

var mariaDBVersionRe = regexp.MustCompile(`(?:^|\s)Ver\s+(\d+\.\d+\.\d+)`) // the server version, not a path

func mariaDBVersionIn(s string) string {
	if m := mariaDBVersionRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// databaseOpenHelp is what the start window shows when the backend cannot
// open the database: the credential-free readiness message, and for an old
// server (nothing secret in it) which version it is and what to install.
func databaseOpenHelp(err error) []string {
	lines := []string{observe.DatabaseHelp}
	var old store.VersionError
	if errors.As(err, &old) {
		lines = append(lines, mariaDBVersionHelp(old.Version))
	}
	return lines
}

// mariaDBVersionHelp tells an old MariaDB apart from other connection
// problems, with what to install.
func mariaDBVersionHelp(version string) string {
	return "이 PC의 MariaDB " + version + "은(는) 지원하지 않습니다. AC2는 MariaDB " + store.MinimumMariaDB + " 이상(같은 계열 안에서)이 필요합니다. " +
		"배포판 저장소나 MariaDB 공식 저장소(https://mariadb.org/download/)에서 그 이상을 설치한 뒤 다시 실행해 주세요. " +
		"시스템 MariaDB를 바꾸면 기존에 쓰던 MariaDB 서비스도 함께 바뀌니, 그 DB를 쓰는 다른 프로그램이 있다면 먼저 백업하고 MariaDB의 업그레이드 안내를 확인해 주세요."
}

func initializeMariaDB(ctx context.Context, dbData, initTool string, c installConfig) error {
	if _, err := os.Stat(filepath.Join(dbData, "mysql")); errors.Is(err, os.ErrNotExist) {
		log.Print("[4/5] MariaDB 전용 데이터 폴더를 초기화합니다.")
		// Only a successful bootstrap is published as the final data directory.
		// An interrupted bootstrap leaves this owned sibling for the next run.
		staging := dbData + ".init"
		if err := os.RemoveAll(staging); err != nil {
			return err
		}
		if err := privateDirectory(staging); err != nil {
			return err
		}
		args := []string{"--datadir=" + staging}
		if runtime.GOOS == "windows" {
			args = append(args, "--silent")
		} else {
			args = append([]string{"--no-defaults"}, append(args, "--auth-root-authentication-method=normal", "--skip-test-db")...)
		}
		cmd := exec.CommandContext(ctx, initTool, args...)
		hideChild(cmd)
		if out, e := cmd.CombinedOutput(); e != nil {
			message := string(out)
			if strings.TrimSpace(message) == "" {
				message = e.Error()
			}
			return fmt.Errorf("MariaDB 초기화 실패: %s", scrubInstallLog(message, c))
		}
		// Earlier versions created the final folder before bootstrapping, so an
		// early failure there left it empty; Windows cannot rename onto it.
		// os.Remove deletes only an empty directory and never removes files.
		if err := os.Remove(dbData); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("MariaDB 데이터 폴더 %s에 이전 파일이 남아 있습니다. 다른 이름으로 옮긴 뒤 다시 실행해 주세요: %w", dbData, err)
		}
		return osfile.Rename(staging, dbData)
	} else if err != nil {
		return err
	} else {
		if err := privateDirectory(dbData); err != nil {
			return err
		}
		log.Print("[4/5] DB 초기화는 이미 준비됨. 계정·스키마를 준비합니다.")
	}
	return nil
}

func scrubInstallLog(s string, c installConfig) string {
	return diagnostics.Redact(s, c.AppPassword, c.RootPassword)
}

func downloadProgressText(received, total int64) string {
	const bytesPerMB = 1_000_000
	if total <= 0 {
		return fmt.Sprintf("[1/5] 내려받기: %.1f MB 받음", float64(received)/bytesPerMB)
	}
	return fmt.Sprintf("[1/5] 내려받기: %.1f / %.1f MB (%.1f%%)", float64(received)/bytesPerMB, float64(total)/bytesPerMB, float64(received)/float64(total)*100)
}

type downloadProgress struct {
	received, total int64
	last            time.Time
}

func (p *downloadProgress) Write(b []byte) (int, error) {
	p.received += int64(len(b))
	if time.Since(p.last) >= time.Second {
		p.print()
	}
	return len(b), nil
}

func (p *downloadProgress) print() {
	log.Print(downloadProgressText(p.received, p.total))
	p.last = time.Now()
}

func windowsMariaDB(ctx context.Context) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		profile, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return "", homeErr
		}
		cache = filepath.Join(profile, "AppData", "Local")
	}
	version := strings.TrimSuffix(strings.TrimPrefix(path.Base(mariaWindowsURL), "mariadb-"), "-winx64.zip")
	dest := filepath.Join(cache, "ArchiveCenter2", "runtime", "mariadb", version)
	return prepareWindowsMariaDB(ctx, dest, mariaWindowsURL, mariaWindowsSHA)
}

func prepareWindowsMariaDB(ctx context.Context, dest, downloadURL, digest string) (string, error) {
	runtimeDir := filepath.Dir(dest)
	if _, err := os.Stat(filepath.Join(dest, "bin", "mariadbd.exe")); err == nil {
		log.Printf("[1/5] 공용 위치의 MariaDB를 사용합니다(%s)", dest)
		log.Print("[2/5] MariaDB 해시 확인: 이미 준비됨.")
		log.Print("[3/5] MariaDB 압축 풀기: 이미 준비됨.")
		return dest, nil
	}
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return "", err
	}
	archive, err := os.CreateTemp(runtimeDir, "mariadb-download-*.zip")
	if err != nil {
		return "", err
	}
	defer os.Remove(archive.Name())
	log.Printf("[1/5] MariaDB를 공용 위치(%s)에 한 번만 내려받습니다", dest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		archive.Close()
		return "", fmt.Errorf("MariaDB 내려받기 요청을 만들지 못했습니다: %w", err)
	}
	response, err := (&http.Client{Timeout: 20 * time.Minute}).Do(req)
	if err != nil {
		archive.Close()
		return "", fmt.Errorf("MariaDB 내려받기 실패. 인터넷 연결을 확인해 주세요: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		archive.Close()
		return "", fmt.Errorf("MariaDB 내려받기 실패: 서버 응답 HTTP %d", response.StatusCode)
	}
	h := sha256.New()
	progress := &downloadProgress{total: response.ContentLength}
	progress.print()
	_, err = io.Copy(io.MultiWriter(archive, h, progress), response.Body)
	closeErr := archive.Close()
	if err != nil {
		return "", fmt.Errorf("MariaDB 내려받기 실패. 인터넷 연결과 디스크 공간을 확인해 주세요: %w", err)
	}
	if closeErr != nil {
		return "", closeErr
	}
	progress.print()
	log.Print("[2/5] 내려받은 MariaDB 파일의 SHA-256 해시 확인을 시작합니다.")
	if hex.EncodeToString(h.Sum(nil)) != digest {
		return "", errors.New("MariaDB 파일의 SHA-256 해시가 공식 배포본과 다릅니다. 시작 파일로 다시 내려받아 주세요")
	}
	log.Print("[2/5] MariaDB 해시 확인 성공.")
	log.Print("[3/5] MariaDB 압축 풀기를 시작합니다.")
	stage, err := os.MkdirTemp(runtimeDir, "mariadb-extract-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage) // stage is an owned directory under runtimeDir
	if err = extractMariaZip(archive.Name(), stage); err != nil {
		return "", err
	}
	if err = osfile.Rename(stage, dest); err != nil {
		if _, existingErr := os.Stat(filepath.Join(dest, "bin", "mariadbd.exe")); existingErr == nil {
			log.Printf("[3/5] 공용 위치의 MariaDB를 사용합니다(%s)", dest)
			return dest, nil
		}
		return "", err
	}
	log.Print("[3/5] MariaDB 압축 풀기 완료.")
	return dest, nil
}

func extractMariaZip(name, dest string) error {
	z, err := zip.OpenReader(name)
	if err != nil {
		return err
	}
	defer z.Close()
	for _, f := range z.File {
		// Official archives have one top-level release directory.
		_, relative, ok := strings.Cut(strings.ReplaceAll(f.Name, "\\", "/"), "/")
		if !ok || relative == "" {
			continue
		}
		path := filepath.Join(dest, filepath.FromSlash(relative))
		r, err := filepath.Rel(dest, path)
		if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(os.PathSeparator)) || filepath.IsAbs(r) {
			return errors.New("MariaDB ZIP 경로가 설치 폴더 밖입니다")
		}
		if f.FileInfo().IsDir() {
			if err = os.MkdirAll(path, 0o700); err != nil {
				return err
			}
			continue
		}
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		in, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			in.Close()
			return err
		}
		_, err = io.Copy(out, in)
		e := out.Close()
		in.Close()
		if err != nil {
			return err
		}
		if e != nil {
			return e
		}
	}
	return nil
}
