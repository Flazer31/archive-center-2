// Command ac2gen replays a stored chat through generation, turn by turn:
// each turn's model input is built from what earlier turns stored (production
// reads), the model is called (or its saved answer reused), and the result is
// stored. Model answers are saved under -cache so reruns do not call again.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"archivecenter2/goservice/internal/embed"
	"archivecenter2/goservice/internal/generate"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

type logRow struct {
	ID        string `json:"id"`
	Session   string `json:"chat_session_id"`
	TurnIndex string `json:"turn_index"`
	Role      string `json:"role"`
	Content   string `json:"content"`
}

type cached struct {
	Model        string  `json:"model"`
	PromptSHA    string  `json:"prompt_sha"`
	Content      string  `json:"content"`
	Thinking     string  `json:"thinking,omitempty"`
	PromptTokens int     `json:"prompt_tokens"`
	OutputTokens int     `json:"output_tokens"`
	Seconds      float64 `json:"seconds"`
}

func main() {
	dsn := flag.String("dsn", "", "MariaDB DSN of the ac2 database")
	logs := flag.String("logs", "", "chat_logs JSON (rows of chat_session_id, turn_index, role, content)")
	from := flag.Int("from", 1, "first turn")
	to := flag.Int("to", 1, "last turn")
	model := flag.String("model", "deepseek-v4.1-flash:cloud", "Ollama model")
	think := flag.String("think", "low", "think setting: low|medium|high|true|false")
	ollama := flag.String("ollama", "http://127.0.0.1:11434", "Ollama URL")
	cacheDir := flag.String("cache", "gen-cache", "saved model answers")
	replayOnly := flag.Bool("replay-only", false, "never call the model; fail on a missing saved answer")
	// for measuring a change of the model input against answers already
	// saved: the turn's newest saved answer is used whatever its prompt was
	// (the stored items then come from those answers, not from new input)
	byTurn := flag.Bool("answers-by-turn", false, "reuse each turn's newest saved answer regardless of the prompt; never call the model")
	reportPath := flag.String("report", "", "write one JSON line per turn (sizes of the model input) to this file")
	embedModel := flag.String("embed-model", "", "Voyage model: index each made turn (key from VOYAGE_API_KEY); empty = no vectors")
	indexAtEnd := flag.Bool("index-at-end", false, "with -embed-model: index every current job once at the end (fewer requests) instead of each turn")
	flag.Parse()

	db, err := store.Open(*dsn)
	if err != nil {
		log.Fatal(err)
	}
	b, err := os.ReadFile(*logs)
	if err != nil {
		log.Fatal(err)
	}
	var rows []logRow
	if err := json.Unmarshal(b, &rows); err != nil {
		log.Fatal(err)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		ti, _ := strconv.Atoi(rows[i].TurnIndex)
		tj, _ := strconv.Atoi(rows[j].TurnIndex)
		if ti != tj {
			return ti < tj
		}
		return rows[i].Role == "user" && rows[j].Role != "user"
	})
	// the chat as the host would show it, turn by turn
	sess := lifecycle.NewSession("chat:replay")
	var host []lifecycle.Row
	turnOf := map[int]*lifecycle.Turn{}
	for _, r := range rows {
		role := "char"
		if r.Role == "user" {
			role = "user"
		}
		host = append(host, lifecycle.Row{Index: len(host), ChatID: "m" + r.ID, Role: role, Data: r.Content})
		if role == "char" {
			sess.Apply(lifecycle.Snapshot{SessionKey: sess.Key, Source: lifecycle.SourceOutputEvent, Rows: append([]lifecycle.Row(nil), host...), TargetIndex: len(host) - 1})
		}
	}
	turns := sess.ActiveTurns()
	for i, t := range turns {
		turnOf[i+1] = t // replay turn number = position
	}
	var thinkVal any = *think
	switch *think {
	case "true":
		thinkVal = true
	case "false":
		thinkVal = false
	}
	cp := &cachedProvider{inner: provider.Ollama{URL: *ollama, Model: *model, Think: thinkVal}, model: *model,
		dir: filepath.Join(*cacheDir, strings.NewReplacer(":", "_", "/", "_").Replace(*model)), byTurn: *byTurn, replayOnly: *replayOnly || *byTurn}
	for n := *from; n <= *to; n++ {
		t := turnOf[n]
		if t == nil {
			log.Fatalf("no turn %d", n)
		}
		src := memory.SourceOf(sess.Key, t)
		// what earlier turns stored, through the production reads
		var earlier []memory.Source
		for m := 1; m < n; m++ {
			earlier = append(earlier, memory.SourceOf(sess.Key, turnOf[m]))
		}
		cp.turn = n
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		r, err := generate.Turn(ctx, db, cp, *model, earlier, src)
		cancel()
		if err != nil {
			log.Fatalf("T%d: %v", n, err)
		}
		// index also a turn made before (a failed index is filled by a rerun)
		if *embedModel != "" && !*indexAtEnd {
			v := provider.Voyage{Model: *embedModel, Key: os.Getenv("VOYAGE_API_KEY")}
			ictx, icancel := context.WithTimeout(context.Background(), 5*time.Minute)
			ir, err := embed.Index(ictx, db, v, *embedModel, []int64{r.Job}, 64)
			icancel()
			fmt.Printf("T%d vectors: memories %d reused %d embedded %d requests %d tokens %d err %v\n", n, ir.Memories, ir.Reused, ir.Embedded, ir.Requests, ir.Tokens, err)
		}
		if r.Done {
			fmt.Printf("T%d done already\n", n)
			continue
		}
		if *reportPath != "" {
			line, _ := json.Marshal(map[string]any{"turn": n, "prompt_bytes": len(r.Prompt), "known_bytes": len(r.Known),
				"known_lines": strings.Count(r.Known, "\n"), "names": len(r.Selection.Names), "memories": len(r.Selection.Memories),
				"stored_memories": r.KnownItems, "answer": filepath.Base(cp.last)})
			f, err := os.OpenFile(*reportPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				log.Fatal(err)
			}
			f.Write(append(line, '\n'))
			f.Close()
		}
		fmt.Printf("T%d prompt %dB known %d items | tokens in %d out %d | %.1fs | entities %d memories %d scenes %d links %d | notes %d %v\n",
			n, len(r.Prompt), len(r.Selection.Names)+len(r.Selection.Memories), r.Result.PromptTokens, r.Result.OutputTokens, r.Result.Seconds,
			len(r.Output.Entities), len(r.Output.Memories), len(r.Output.Scenes), len(r.Output.Links), len(r.Notes), r.Notes)
	}
	if *embedModel != "" && *indexAtEnd {
		var srcs []memory.Source
		for n := 1; n <= *to; n++ {
			srcs = append(srcs, memory.SourceOf(sess.Key, turnOf[n]))
		}
		jobs, err := db.CurrentJobs(srcs)
		if err != nil {
			log.Fatal(err)
		}
		v := provider.Voyage{Model: *embedModel, Key: os.Getenv("VOYAGE_API_KEY")}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		ir, err := embed.Index(ctx, db, v, *embedModel, jobs, 64)
		cancel()
		fmt.Printf("vectors T1-T%d: jobs %d memories %d reused %d embedded %d requests %d tokens %d err %v\n", *to, len(jobs), ir.Memories, ir.Reused, ir.Embedded, ir.Requests, ir.Tokens, err)
	}
}

// cachedProvider answers from saved answers, calling the model (and saving
// its answer) only for a prompt not saved before.
type cachedProvider struct {
	inner      generate.Provider
	model, dir string
	byTurn     bool // the turn's newest saved answer, whatever its prompt
	replayOnly bool
	turn       int
	last       string // the answer file used last
}

func (c *cachedProvider) Chat(ctx context.Context, prompt string) (provider.Result, error) {
	sum := sha256.Sum256([]byte(prompt))
	sha := hex.EncodeToString(sum[:8])
	c.last = filepath.Join(c.dir, fmt.Sprintf("T%03d-%s.json", c.turn, sha))
	if c.byTurn {
		c.last = newestAnswer(c.dir, c.turn)
	}
	var saved cached
	if b, err := os.ReadFile(c.last); err == nil {
		if err := json.Unmarshal(b, &saved); err != nil {
			return provider.Result{}, err
		}
		return provider.Result{Content: saved.Content, Thinking: saved.Thinking, PromptTokens: saved.PromptTokens, OutputTokens: saved.OutputTokens, Seconds: saved.Seconds}, nil
	}
	if c.replayOnly {
		return provider.Result{}, fmt.Errorf("no saved answer %s", c.last)
	}
	res, err := c.inner.Chat(ctx, prompt)
	if err != nil {
		return res, err
	}
	os.MkdirAll(c.dir, 0o755)
	b, _ := json.MarshalIndent(cached{Model: c.model, PromptSHA: sha, Content: res.Content, Thinking: res.Thinking, PromptTokens: res.PromptTokens, OutputTokens: res.OutputTokens, Seconds: res.Seconds}, "", " ")
	return res, os.WriteFile(c.last, b, 0o644)
}

// newestAnswer is the most recently saved answer of turn n.
func newestAnswer(dir string, n int) string {
	files, _ := filepath.Glob(filepath.Join(dir, fmt.Sprintf("T%03d-*.json", n)))
	best, bestTime := "", time.Time{}
	for _, f := range files {
		if st, err := os.Stat(f); err == nil && st.ModTime().After(bestTime) {
			best, bestTime = f, st.ModTime()
		}
	}
	return best
}
