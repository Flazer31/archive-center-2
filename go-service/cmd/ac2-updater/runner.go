package main

// The start scripts run a preserved updater copy. This process is outside the
// managed parent's Job; it waits for that parent's DB shutdown and exit before
// replacing either installed executable.
import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"archivecenter2/goservice/internal/datapath"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/packageupdate"
	"archivecenter2/goservice/internal/serve"
	"archivecenter2/goservice/internal/updateapi"
)

func supervise(root string, args []string, book *startupLog) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	data, args, err := resolveBackendData(root, args)
	if err != nil {
		return err
	}
	runner, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	for {
		log.Print("[시작] 보류된 업데이트 확인")
		applied, applyErr := packageupdate.ApplyPendingFromRunner(root, runner)
		if applyErr != nil {
			// ApplyPending already restores every journaled program file when an
			// apply fails. Keep its diagnostic and run the restored installation.
			log.Printf("업데이트 적용 실패: %v", applyErr)
			if _, err := packageupdate.Rollback(root); err != nil {
				return err
			}
		}
		cmd, done, startErr := startManaged(root, args, book)
		if applied.HealthRequired {
			if startErr == nil {
				startErr = waitReady(ctx, data, args, applied.TargetVersion, done)
			}
			if startErr == nil {
				_, startErr = packageupdate.Commit(root)
				if startErr != nil {
					// Commit first persists the committed state, then removes its
					// backup. A cleanup error cannot undo that durable decision.
					state, stateErr := packageupdate.Status(root)
					if stateErr == nil && state.Status == "committed" {
						log.Printf("업데이트 확정 후 정리 보류: %v", startErr)
						startErr = nil
					}
				}
				if startErr == nil {
					log.Printf("업데이트 확정: %s", applied.TargetVersion)
				}
			}
			if startErr != nil {
				if cmd != nil {
					stopManaged(root, data, cmd, done, book)
				}
				if _, err := packageupdate.Rollback(root); err != nil {
					return fmt.Errorf("update failed (%v); rollback: %w", startErr, err)
				}
				log.Printf("새 버전 시작 실패. 이전 프로그램 복원: %s", applied.CurrentVersion)
				if ctx.Err() != nil {
					return nil
				}
				cmd, done, err = startManaged(root, args, book)
				if err != nil {
					return err
				}
				if err := waitReady(ctx, data, args, applied.CurrentVersion, done); err != nil {
					stopManaged(root, data, cmd, done, book)
					return fmt.Errorf("restored backend readiness: %w", err)
				}
				log.Print("이전 버전 재시작 확인 완료")
			}
		} else if startErr != nil {
			return startErr
		}
		select {
		case <-ctx.Done():
			stopManaged(root, data, cmd, done, book)
			return nil
		case err := <-done:
			if exitCode(err) == updateapi.ApplyExitCode {
				// Wait completed after runManaged's deferred SQL SHUTDOWN,
				// install lock release and Windows Job teardown.
				continue
			}
			return err
		}
	}
}

func startManaged(root string, args []string, book *startupLog) (*exec.Cmd, chan error, error) {
	log.Print("[시작] 백엔드 실행")
	name := "ac2"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	cmd := exec.Command(filepath.Join(root, "bin", name), append(args, "-update-managed")...)
	cmd.Dir = root
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if book != nil {
		cmd.Stdout, cmd.Stderr = book.out, book.err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return cmd, done, nil
}

func stopManaged(root, data string, cmd *exec.Cmd, done chan error, book *startupLog) {
	select {
	case err := <-done:
		done <- err
		return
	default:
	}
	if runtime.GOOS == "windows" {
		// Use the existing normal stop owner: only the installation's backend
		// exits; its managed parent remains alive to shut down MariaDB.
		stop := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(root, "bin", "stop.ps1"), "-Data", data)
		stop.Stdout, stop.Stderr = os.Stdout, os.Stderr
		if book != nil {
			stop.Stdout, stop.Stderr = book.out, book.err
		}
		_ = stop.Run()
	} else {
		_ = cmd.Process.Signal(os.Interrupt)
	}
	// The manager owns bounded DB shutdown. Never kill it before that cleanup.
	err := <-done
	done <- err
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exited *exec.ExitError
	if errors.As(err, &exited) {
		return exited.ExitCode()
	}
	return -1
}

func argumentValue(args []string, name, fallback string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-"+name || arg == "--"+name {
			if i+1 < len(args) {
				fallback = args[i+1]
				i++
			}
		} else if key, value, ok := strings.Cut(arg, "="); ok && (key == "-"+name || key == "--"+name) {
			fallback = value
		}
	}
	return fallback
}

func waitReady(ctx context.Context, data string, args []string, expected string, done chan error) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second) // AC1 launcher health window
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	cert := argumentValue(args, "tls-cert", "")
	if cert != "" {
		pem, err := os.ReadFile(cert)
		if err != nil {
			return err
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		pool.AppendCertsFromPEM(pem)
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
	}
	for {
		if err := probeReady(ctx, client, data, args, expected); err == nil {
			return nil
		}
		select {
		case err := <-done:
			done <- err
			return errors.New("managed process exited before readiness")
		case <-ctx.Done():
			return errors.New("backend readiness timeout or launcher stopped")
		case <-time.After(time.Second):
		}
	}
}

func probeReady(ctx context.Context, client *http.Client, data string, args []string, expected string) error {
	port, err := strconv.ParseInt(argumentValue(args, "port", strconv.Itoa(serve.DefaultPort)), 0, strconv.IntSize)
	if err != nil {
		return err
	}
	token, err := os.ReadFile(filepath.Join(data, "token.txt"))
	if err != nil {
		return errors.New("readiness token unavailable")
	}
	listen := argumentValue(args, "listen", serve.DefaultListen)
	if strings.TrimSpace(listen) == "" {
		listen = serve.DefaultListen
	}
	host := strings.TrimSpace(strings.Split(listen, ",")[0])
	if host == "all" || host == "0.0.0.0" {
		host = "127.0.0.1"
	} else if host == "::" {
		host = "::1"
	}
	scheme := "http"
	if argumentValue(args, "tls-cert", "") != "" {
		scheme = "https"
	}
	base := scheme + "://" + net.JoinHostPort(host, strconv.FormatInt(port, 10))
	for _, route := range []string{"/ready", "/version"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+route, nil)
		if err != nil {
			return err
		}
		req.Header.Set(observe.TokenHeader, strings.TrimSpace(string(token)))
		resp, err := client.Do(req)
		if err != nil {
			return errors.New("readiness connection unavailable")
		}
		var value struct {
			Status  string `json:"status"`
			Version string `json:"machine_version"`
		}
		err = json.NewDecoder(resp.Body).Decode(&value)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			return errors.New("readiness response unavailable")
		}
		if route == "/ready" && value.Status != "ready" {
			return errors.New("backend not ready")
		}
		if route == "/version" && value.Version != expected {
			return errors.New("backend version differs from update target")
		}
	}
	return nil
}

// Resolve once before launching; restarts and probes retain this same path.
// Scan the backend flag prefix with Go flag semantics.
func resolveBackendData(root string, args []string) (string, []string, error) {
	explicit := ""
	kept := make([]string, 0, len(args)+2)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" || arg == "-" || !strings.HasPrefix(arg, "-") {
			kept = append(kept, args[i:]...)
			break
		}
		key, value, equal := strings.Cut(strings.TrimPrefix(arg, "-"), "=")
		key = strings.TrimPrefix(key, "-")
		if key == "data" {
			if !equal {
				if i+1 >= len(args) {
					kept = append(kept, arg)
					continue
				}
				i++
				value = args[i]
			}
			explicit = value
			continue
		}
		kept = append(kept, arg)
		if !equal && key != "managed" && key != "update-managed" && i+1 < len(args) {
			i++
			kept = append(kept, args[i])
		}
	}
	data, err := datapath.Resolve(explicit, root)
	if err != nil {
		return "", nil, err
	}
	normalized := append([]string{"-data", data}, kept...)
	return data, normalized, nil
}
