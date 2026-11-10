package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"archivecenter2/goservice/internal/datapath"
	"archivecenter2/goservice/internal/diagnostics"
	"archivecenter2/goservice/internal/observe"
)

// Only operational port metadata is exported. install.json also contains DB
// passwords: never marshal its source map or copy its original bytes.
type diagnosticInstall struct {
	Status       string `json:"status"`
	BackendPort  int    `json:"backend_port,omitempty"`
	DatabasePort int    `json:"database_port,omitempty"`
}

type offlineDiagnosticReport struct {
	diagnostics.Report
	Collector string            `json:"collector"`
	Install   diagnosticInstall `json:"install"`
	// what a virus scanner may see in the package (1.0 O10, read only)
	Trust *diagnostics.TrustReport `json:"windows_trust,omitempty"`
}

func collectInstallMetadata(data string) diagnosticInstall {
	name := filepath.Join(data, "install.json")
	info, err := os.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return diagnosticInstall{Status: "missing"}
	}
	if err != nil || !info.Mode().IsRegular() {
		return diagnosticInstall{Status: "unavailable"}
	}
	const maximum = 64 * 1024
	if info.Size() > maximum {
		return diagnosticInstall{Status: "too_large"}
	}
	f, err := os.Open(name)
	if err != nil {
		return diagnosticInstall{Status: "unavailable"}
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maximum+1))
	if err != nil {
		return diagnosticInstall{Status: "unavailable"}
	}
	if len(b) > maximum {
		return diagnosticInstall{Status: "too_large"}
	}
	var ports struct {
		BackendPort  int `json:"backend_port"`
		DatabasePort int `json:"database_port"`
	}
	if json.Unmarshal(b, &ports) != nil {
		return diagnosticInstall{Status: "invalid"}
	}
	return diagnosticInstall{Status: "available", BackendPort: ports.BackendPort, DatabasePort: ports.DatabasePort}
}

func exportOfflineDiagnostics(root, explicitData string) (string, error) {
	data, err := datapath.Resolve(explicitData, root)
	if err != nil {
		return "", err
	}
	report := offlineDiagnosticReport{
		Report:    diagnostics.NewReport(data, observe.ServiceVersion, time.Time{}),
		Collector: "updater_offline",
		Install:   collectInstallMetadata(data),
		Trust:     diagnostics.PackageTrust(root, data),
	}
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return "", err
	}
	directory := filepath.Join(data, "diagnostics")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(directory, "ac2-diagnostics-"+time.Now().UTC().Format("20060102-150405")+"-*.json")
	if err != nil {
		return "", err
	}
	name := f.Name()
	_, writeErr := f.Write(append(body, '\n'))
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(name)
		return "", errors.Join(writeErr, closeErr)
	}
	return name, nil
}
