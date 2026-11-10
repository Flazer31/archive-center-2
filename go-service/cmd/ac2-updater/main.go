package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"strings"

	"archivecenter2/goservice/internal/diagnostics"
	"archivecenter2/goservice/internal/packageupdate"
)

type errorResult struct {
	ContractVersion string `json:"contract_version"`
	Action          string `json:"action"`
	Status          string `json:"status"`
	Code            string `json:"code"`
	Message         string `json:"message"`
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	var book *startupLog
	stderr := io.Writer(os.Stderr)
	if name := os.Getenv("AC2_START_LOG"); name != "" && (len(args) == 0 || args[0] != "diagnostics") {
		var err error
		book, err = openStartupLog(name, os.Stdout, os.Stderr)
		if err != nil {
			fmt.Fprintln(os.Stderr, "시작 로그를 열지 못했습니다:", err)
		} else {
			defer book.close()
			if len(args) > 0 && args[0] == "run" {
				stopProgress := book.progress()
				defer stopProgress()
			}
			stderr = book.err
			previous := log.Writer()
			// one record per log call, as the backend writes (review 04)
			log.SetOutput(diagnostics.IndentedLog(stderr))
			defer log.SetOutput(previous)
		}
	}
	failure := func(action, code string, err error) int {
		if book != nil {
			book.failure(err)
		}
		status := writeFailureTo(stderr, action, code, err)
		if childCode := exitCode(err); childCode > 0 {
			status = childCode
		}
		return status
	}
	if len(args) == 0 {
		return failure("", "usage", fmt.Errorf("command is required"))
	}
	action := args[0]
	flags := flag.NewFlagSet(action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	root := flags.String("root", "", "AC2 package root")
	data := flags.String("data", "", "persistent data directory (diagnostics)")
	version := flags.String("version", "", "machine package version (manifest build)")
	files := flags.String("files", "", "comma-separated build-owned files (manifest build)")
	if err := flags.Parse(args[1:]); err != nil {
		return failure(action, "usage", err)
	}
	if (flags.NArg() != 0 && action != "run" && action != "data-path") || *root == "" {
		return failure(action, "usage", fmt.Errorf("--root is required and positional arguments are not accepted"))
	}
	var (
		result packageupdate.Result
		err    error
	)
	switch action {
	case "diagnostics":
		name, exportErr := exportOfflineDiagnostics(*root, *data)
		if exportErr != nil {
			return failure(action, "diagnostics_failed", exportErr)
		}
		fmt.Fprintln(os.Stdout, "진단 보고서를 저장했습니다:", name)
		return 0
	case "data-path":
		data, _, resolveErr := resolveBackendData(*root, flags.Args())
		if resolveErr != nil {
			return failure(action, "data_path_failed", resolveErr)
		}
		fmt.Fprintln(os.Stdout, data)
		return 0
	case "run":
		if err := supervise(*root, flags.Args(), book); err != nil {
			return failure(action, "launcher_failed", err)
		}
		fmt.Fprintln(stderr, "정상 종료됨. 종료 코드: 0")
		return 0
	case "manifest":
		if err := packageupdate.WritePackageManifest(*root, *version, strings.Split(*files, ",")); err != nil {
			return failure(action, "manifest_failed", err)
		}
		return 0
	case "prepare-runner":
		runnerPath, prepareErr := packageupdate.PrepareRunner(*root)
		if prepareErr != nil {
			return failure(action, "runner_prepare_failed", prepareErr)
		}
		fmt.Fprintln(os.Stdout, runnerPath)
		return 0
	case "apply-pending":
		if runtime.GOOS == "windows" {
			runnerPath, executableErr := os.Executable()
			if executableErr != nil {
				return failure(action, "runner_identity_invalid", executableErr)
			}
			result, err = packageupdate.ApplyPendingFromRunner(*root, runnerPath)
		} else {
			result, err = packageupdate.ApplyPending(*root)
		}
	case "commit":
		result, err = packageupdate.Commit(*root)
	case "rollback":
		result, err = packageupdate.Rollback(*root)
	case "status":
		result, err = packageupdate.Status(*root)
	default:
		return failure(action, "usage", fmt.Errorf("unsupported command %q", action))
	}
	if err != nil {
		code := "update_failed"
		var updateError *packageupdate.UpdateError
		if errors.As(err, &updateError) {
			code = updateError.Code
		}
		return failure(action, code, err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		return failure(action, "output_failed", err)
	}
	return 0
}

func writeFailure(action, code string, err error) int {
	return writeFailureTo(os.Stderr, action, code, err)
}

func writeFailureTo(w io.Writer, action, code string, err error) int {
	_ = json.NewEncoder(w).Encode(errorResult{
		ContractVersion: packageupdate.ResultContract,
		Action:          action,
		Status:          "error",
		Code:            code,
		Message:         err.Error(),
	})
	return 1
}
