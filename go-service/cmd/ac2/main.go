// Command ac2 runs the AC2 backend. It listens on all addresses by default;
// -listen can restrict it to chosen IPs. Every request carries the access token kept in <data>/token.txt
// (made on first start). -tls-cert/-tls-key serve HTTPS.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"archivecenter2/goservice/internal/datapath"
	"archivecenter2/goservice/internal/diagnostics"
	"archivecenter2/goservice/internal/embed"
	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/outbounddns"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/secret"
	"archivecenter2/goservice/internal/serve"
	"archivecenter2/goservice/internal/store"
	"archivecenter2/goservice/internal/updateapi"
)

func main() {
	os.Exit(runBackend())
}

func runBackend() int {
	log.SetOutput(diagnostics.IndentedLog(os.Stderr))
	port := flag.Int("port", serve.DefaultPort, "fixed backend port")
	listen := flag.String("listen", serve.DefaultListen, "comma separated listen IPs, or all (default); 127.0.0.1 for this PC only")
	tlsCert := flag.String("tls-cert", "", "HTTPS certificate file (with -tls-key)")
	tlsKey := flag.String("tls-key", "", "HTTPS key file")
	dataDir := flag.String("data", "", "persistent data directory (default: per-user shared directory)")
	dsn := flag.String("mariadb-dsn", "", "MariaDB DSN for the ac2 database; empty = observation log file only")
	// memory generation until the settings screen saves a critic; empty model = none
	genModel := flag.String("gen-model", "", "Ollama model for memory generation, e.g. deepseek-v4.1-flash:cloud; empty = no generation")
	genURL := flag.String("gen-ollama", "http://127.0.0.1:11434", "Ollama URL for memory generation")
	genThink := flag.String("gen-think", "low", "generation think setting: low|medium|high|true|false")
	genRetries := flag.Int("gen-retries", 3, "model calls a turn gets again after a failed one (waits 1, 5, 30 minutes)")
	// memory vectors (Voyage AI; key from the VOYAGE_API_KEY environment variable); empty = off
	embedModel := flag.String("embed-model", "voyage-context-4", "Voyage model for memory vectors: voyage-context-4 (a turn's memories embedded with the turn as context) or another, e.g. voyage-4-large; empty = no vectors")
	managed := flag.Bool("managed", false, "start the package's own MariaDB and backend together")
	updateRepo := flag.String("update-repo", os.Getenv("AC2_UPDATE_GITHUB_REPO"), "update repository owner/name; empty = not configured")
	updateAPI := flag.String("update-api", os.Getenv("AC2_UPDATE_API_BASE"), "GitHub Releases API base URL")
	updateAware := flag.Bool("update-managed", false, "internal: update-aware package launcher")
	flag.Parse()
	base, err := os.Getwd()
	if *managed {
		var executable string
		executable, err = os.Executable()
		base = filepath.Dir(executable)
	}
	if err == nil {
		*dataDir, err = datapath.Resolve(*dataDir, base)
	}
	if err != nil {
		log.Print(err)
		return 1
	}
	log.Printf("데이터 폴더: %s", *dataDir)
	outbounddns.ConfigureOutboundDNSServers(os.Getenv("AC2_DNS_SERVERS"))
	if *managed {
		if err := runManaged(*dataDir, *port, *listen, *tlsCert, *tlsKey, *updateRepo, *updateAPI, *updateAware); err != nil {
			var exited *exec.ExitError
			if errors.As(err, &exited) {
				return exited.ExitCode()
			}
			log.Print(err)
			return 1
		}
		return 0
	}
	cfg := serve.Config{Listen: *listen, Port: *port, TLSCert: *tlsCert, TLSKey: *tlsKey}
	if err := cfg.CheckAvailable(); err != nil {
		log.Print(err)
		return 1
	}
	if *dsn == "" {
		b, err := os.ReadFile(filepath.Join(*dataDir, "mariadb.dsn"))
		if err == nil {
			*dsn = strings.TrimSpace(string(b))
		} else if !errors.Is(err, os.ErrNotExist) {
			log.Printf("DB 연결 파일을 읽지 못했습니다. 패키지 시작 파일로 다시 실행해 주세요.")
		}
	}

	var server *observe.Server
	var db *store.DB
	var reportModelSecrets func() []string
	if *dsn != "" {
		var err error
		log.Print("[시작] DB 연결·스키마 준비")
		db, err = store.Open(*dsn)
		if err != nil {
			// DSNs and connection errors can contain credentials. The plugin
			// gets the same actionable, credential-free readiness message.
			log.Print(observe.DatabaseHelp)
		}
	}
	if db != nil {
		defer db.Close()
		log.Print("[4/5] DB 초기화·계정·스키마 준비 완료.")
		server = observe.NewServer(store.RawSink{DB: db})
		pipe := pipeline.New(db)
		server.After = pipe
		log.Printf("storing in MariaDB")
		// API keys are kept sealed with a key file of the backend's own
		box, err := secret.LoadBox(filepath.Join(*dataDir, "secret.key"))
		if err != nil {
			log.Fatalf("key file: %v", err)
		}
		// the generation worker always runs; it waits while no critic is set
		worker := generate.NewWorker(db, nil, "", 10*time.Minute)
		decision := decisionHandler(db, box) // one save lock for both paths
		mods := &models{db: db, box: box, worker: worker, flagEmbed: *embedModel, envKey: os.Getenv("VOYAGE_API_KEY"), flagRetries: *genRetries}
		if *genModel != "" {
			reasoning := *genThink
			switch reasoning {
			case "true":
				reasoning = ""
			case "false":
				reasoning = "off"
			}
			mods.flagCritic = provider.Config{Provider: "ollama", Model: *genModel, URL: *genURL, Reasoning: reasoning}
		}
		saved, err := db.ModelSettings(box)
		if err != nil {
			log.Fatalf("model settings: %v", err)
		}
		mods.apply(saved, false)
		worker.Index = func(ctx context.Context, jobs []int64) error {
			v := mods.query()
			if v == nil {
				return nil // no vectors without a key; filled once one is set
			}
			_, err := embed.Index(ctx, db, v, v.VectorModel(), jobs, 64)
			return err
		}
		st := newStatus(db, mods, worker)
		server.ReportSettings = mods.diagnosticSettings
		server.ReportFailures = st.diagnosticFailures
		reportModelSecrets = mods.diagnosticSecrets
		worker.Event = st.workerEvent
		hud := newHUD(db, pipe, worker, st)
		cold, clearCold := backfillHandlers(pipe, worker, db, server) // one run list for both paths
		maint := &maintenance{db: db, mods: mods, worker: worker, pipe: pipe, server: server, status: st, clearCold: clearCold}
		server.Routes = map[string]http.HandlerFunc{
			"/maintenance/reindex":       maint.reindex,
			"/maintenance/reset":         maint.reset,
			"/hud":                       hud.handler(),
			"/hud/retry":                 hud.handler(),
			"/settings/hud":              hud.handler(),
			"/status":                    st.handler(),
			"/injection":                 st.injectionHandler(),
			"/injection/trace":           st.traceHandler(),
			"/memory/inject":             injectHandler(server, pipe, db, mods.query, st),
			"/settings/memory":           settingsHandler(db),
			"/settings/models":           mods.handler(),
			"/settings/decision":         decision,
			"/settings/decision/test":    decision,
			"/settings/preprocess":       preprocessHandler(mods),
			"/settings/narrative":        narrativeHandler(mods),
			"/settings/prompt":           promptHandler(db),
			"/settings/publisher-prompt": publisherPromptHandler(db),
			"/memory/backfill":           cold,
			"/memory/backfill/cancel":    cold,
			"/memory/hypa":               hypaHandler(pipe, worker, db, server),
			"/memory/list":               memoriesHandler(pipe, db, worker, st),
			"/memory/detail":             memoriesHandler(pipe, db, worker, st),
			"/memory/edit":               memoriesHandler(pipe, db, worker, st),
			"/memory/delete":             memoriesHandler(pipe, db, worker, st),
			"/memory/hold":               memoriesHandler(pipe, db, worker, st),
			"/worldline":                 worldlineHandler(pipe, db),
			"/worldline/turn":            worldlineHandler(pipe, db),
			"/chats":                     chatsHandler(db),
			"/chats/compare":             chatCompareHandler(db),
			"/chats/delete":              chatOpsHandler(pipe, db, worker, st),
			"/chats/copy":                chatOpsHandler(pipe, db, worker, st),
			"/chats/move":                chatOpsHandler(pipe, db, worker, st),
			"/chats/attach":              attachHandler(pipe, db, worker),
			"/body":                      bodyHandler(pipe, db),
			"/body/settings":             bodyHandler(pipe, db),
			"/body/person":               bodyHandler(pipe, db),
			"/body/record":               bodyHandler(pipe, db),
			"/body/record/delete":        bodyHandler(pipe, db),
			"/chats/turns":               turnsHandler(pipe, db, worker, st),
			"/memory/turn/clear":         turnsHandler(pipe, db, worker, st),
			"/memory/turn/remake":        turnsHandler(pipe, db, worker, st),
			"/memory/turn/remake-failed": remakeFailedHandler(pipe, db, worker, st),
			"/names":                     namesHandler(pipe, db, worker),
			"/names/edit":                namesHandler(pipe, db, worker),
			"/names/merge":               namesHandler(pipe, db, worker),
			"/names/unmerge":             namesHandler(pipe, db, worker),
			"/names/merge/preview":       namesPreviewHandler(db),
		}
		pipe.Changed = func(s *lifecycle.Session) { worker.Notify(s.Key, memory.WithPast(s)) }
		pipe.Label = func(key, charName, chatName string) {
			if err := db.SetChatLabel(key, charName, chatName); err != nil {
				log.Printf("chat label %s: %v", key, err)
			}
		}
		// after a restart, every stored chat goes on by itself (turns left to
		// make, kept answers, missing vectors), independently per chat
		if err := worker.Resume(); err != nil {
			log.Printf("memory generation: resume: %v", err)
		}
		go worker.Run(context.Background())
		server.After = withGeneration{pipe, worker}
		view := mods.view()["critic"].(map[string]any)
		log.Printf("memory generation: %v %v (%v)", view["provider"], view["model"], view["source"])
	} else {
		name := "observations-" + time.Now().Format("20060102-150405") + ".jsonl"
		sink, err := observe.NewFileSink(filepath.Join(*dataDir, "observations"), name)
		if err != nil {
			log.Fatalf("sink: %v", err)
		}
		server = observe.NewServer(sink)
		// These are existing database-backed routes. Report the missing
		// dependency instead of letting a settings screen encounter a 404.
		server.Routes = map[string]http.HandlerFunc{"/": observe.DatabaseUnavailable}
		log.Printf("observation log %s", sink.Path())
	}
	server.DataDirectory = *dataDir
	server.DatabaseConfigured = *dsn != ""
	server.ReportSecrets = func() []string {
		values := []string{*dsn}
		if reportModelSecrets != nil {
			values = append(values, reportModelSecrets()...)
		}
		return values
	}
	server.DatabaseReady = func() error {
		if db == nil {
			return errors.New("database not connected")
		}
		return (store.RawSink{DB: db}).Ready()
	}
	tokenPath := filepath.Join(*dataDir, "token.txt")
	tok, made, err := observe.LoadToken(tokenPath)
	if err != nil {
		log.Fatalf("access token: %v", err)
	}
	server.Token = tok
	shutdown := make(chan int, 1)
	executable, err := os.Executable()
	if err != nil {
		log.Print(err)
		return 1
	}
	updates := updateapi.Config{Root: filepath.Dir(filepath.Dir(executable)), Repository: *updateRepo, APIBase: *updateAPI, CurrentVersion: observe.MachineVersion}
	if *updateAware {
		updates.RequestShutdown = func(code int) {
			select {
			case shutdown <- code:
			default:
			}
		}
	}
	for route, handler := range updateapi.New(updates).Routes() {
		server.Routes[route] = handler
	}
	last4, _ := tok.Info()
	if made {
		// shown in plain text this once only, when the token is made (user
		// decisions 10-27, 10-28): after this only its end is printed
		log.Printf("새 접속 토큰이 만들어졌습니다. 이 화면에서 한 번만 보여 줍니다. 지금 반드시 복사해 플러그인 설정 → 연결에 넣고, 안전한 곳에 보관하세요:\n\n    %s\n\n(파일: %s)", tok.Value(), tokenPath)
	} else {
		log.Printf("access token: %s (ends with %s)", tokenPath, last4)
	}
	if !tok.Checked() {
		log.Printf("접속 토큰 확인이 꺼져 있습니다: 토큰 없이도 접속됩니다 (설정 → 연결에서 켤 수 있어요)")
	}
	connection, err := cfg.ConnectionInfo()
	if err != nil {
		log.Printf("접속 주소 목록을 읽지 못했습니다: %v", err)
	}
	server.Connection = connection
	addrs, err := cfg.Addrs()
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	servers, _, errs, err := serve.Start(cfg, server.Public())
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	scheme := "http"
	if *tlsCert != "" {
		scheme = "https"
	}
	log.Printf("[5/5] %s %s listening on %s (%s)", observe.ServiceName, observe.ServiceVersion, strings.Join(addrs, ", "), scheme)
	log.Printf("[5/5] AC2 시작: DB 연결 후 플러그인에서 연결 시험을 해 주세요. 종료: Ctrl+C 또는 종료 파일. 데이터: %s", *dataDir)
	if connection.LocalURL != "" {
		log.Printf("이 PC: %s", connection.LocalURL)
	}
	log.Print(connection.RemoteHint)
	if runtime.GOOS == "windows" {
		log.Print("Windows에서 처음 시작할 때 방화벽 허용 창이 뜰 수 있어요. 다른 기기에서 연결하려면 허용해 주세요.")
	}
	log.Print("[준비 완료] 백엔드가 실행 중입니다. 위 주소로 플러그인에서 연결 시험을 해 주세요.")
	select {
	case code := <-shutdown:
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, httpServer := range servers {
			if err := httpServer.Shutdown(ctx); err != nil {
				log.Printf("HTTP shutdown: %v", err)
			}
		}
		return code
	case err := <-errs:
		log.Print(err)
		return 1
	}
}

// withGeneration adds the generation counts to the lifecycle diagnostics.
type withGeneration struct {
	*pipeline.Pipeline
	w *generate.Worker
}

func (g withGeneration) Diagnostics() map[string]any {
	d := g.Pipeline.Diagnostics()
	for k, v := range g.w.Diagnostics() {
		d[k] = v
	}
	return d
}
