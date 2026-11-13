package main

import (
	"archivecenter2/goservice/internal/ingest"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pdfmemory"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	pdfreader "github.com/ledongthuc/pdf"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestPDFTransportPreservesSelectedDelivery(t *testing.T) {
	d := store.Delivery{Block: "[기억]\n- 첫째 조건: 승인 후 실행\n- 둘째: 유채(油菜)", IDs: []string{"second", "first"}, Chars: 39, SecretChars: 12}
	before := d
	before.IDs = append([]string(nil), d.IDs...)
	tr := selectedMemoryTransport("google_pdf", d.Block, nil)
	if tr == nil || tr.Status != "ready" || tr.Text != d.Block || tr.MIMEType != "application/pdf" {
		t.Fatalf("transport: %+v", tr)
	}
	pdf, err := base64.StdEncoding.DecodeString(tr.PDFBase64)
	if err != nil || !strings.HasPrefix(string(pdf), "%PDF-") {
		t.Fatal("missing PDF", err)
	}
	if !reflect.DeepEqual(d, before) {
		t.Fatal("conversion changed selection, order, block or budgets")
	}
	if selectedMemoryTransport("text", d.Block, nil) != nil || selectedMemoryTransport("google_pdf", "", nil) != nil {
		t.Fatal("default/empty must not attach PDF")
	}
}

func TestPDFTransportFailureRetainsExactText(t *testing.T) {
	for _, block := range []string{"memory\x00unrenderable", string([]byte{0xff})} {
		tr := selectedMemoryTransport("google_pdf", block, nil)
		if tr == nil || tr.Status != "failed" || tr.Text != block || tr.PDFBase64 != "" || tr.Error == "" {
			t.Fatalf("failure lost original: %+v", tr)
		}
	}
}

func TestPDFTransportSettingsPersistIndependently(t *testing.T) {
	db := modelsDB(t)
	h := settingsHandler(db)
	code, v, raw := callModels(t, h, "GET", "")
	if code != 200 || v["transport_mode"] != "text" {
		t.Fatal(code, raw)
	}
	code, v, raw = callModels(t, h, "POST", `{"transport_mode":"google_pdf","memory_chars":1357,"secret_chars":2468}`)
	if code != 200 || v["transport_mode"] != "google_pdf" {
		t.Fatal(code, raw)
	}
	reopened, err := store.Open(os.Getenv("AC2_TEST_MARIADB_DSN") + "ac2_test_models?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	mode, err := reopened.MemoryTransport()
	if err != nil || mode != "google_pdf" {
		t.Fatal(mode, err)
	}
	code, v, raw = callModels(t, h, "POST", `{"memory_chars":999}`)
	if code != 200 || v["transport_mode"] != "google_pdf" || v["secret_chars"] != float64(2468) {
		t.Fatal(code, raw)
	}
	code, v, raw = callModels(t, h, "POST", `{"transport_mode":"unsupported"}`)
	if code != 200 || v["transport_mode"] != "text" || v["memory_chars"] != float64(999) {
		t.Fatal(code, raw)
	}
}

// Actual handlers, selection and DB; provider boundary is disabled.
func TestPDFTransportInjectKeepsSelection(t *testing.T) {
	db := modelsDB(t)
	pipe := pipeline.New(db)
	event := json.RawMessage(`{"boot_id":"pdf","seq":1,"hook":"load","stored_chat":{"chat_id":"pdf-chat","rows":[{"i":0,"role":"user","chat_id":"u1","data":{"text":"earlier input"}},{"i":1,"role":"char","chat_id":"a1","data":{"text":"earlier response"}},{"i":2,"role":"user","chat_id":"u2","data":{"text":"middle input"}},{"i":3,"role":"char","chat_id":"a2","data":{"text":"middle response"}},{"i":4,"role":"user","chat_id":"u3","data":{"text":"current input"}},{"i":5,"role":"char","chat_id":"a3","data":{"text":"current response"}}]}}`)
	if _, err := pipe.Process([]observe.Record{{Event: event}}); err != nil {
		t.Fatal(err)
	}
	var src memory.Source
	pipe.WithSession("chat:pdf-chat", func(s *lifecycle.Session) { src = memory.SourceOf(s.Key, s.ActiveTurns()[0]) })
	job, err := db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	answer := `{"entities":[{"id":"n1","name":"Rina"},{"id":"n2","name":"Doyun"}],"memories":[{"id":"m1","kind":"fact","about":"n1","slot":"profession","body":"Rina is a pharmacist"},{"id":"m2","kind":"fact","about":"n1","slot":"condition","body":"Rina is a princess after royal confirmation","hide_from":["n2"]}]}`
	out, _ := memory.Parse(answer)
	if _, err = db.SaveGeneration(job, store.Generation{Request: "synthetic", Response: answer, Output: out}); err != nil {
		t.Fatal(err)
	}
	inject := injectHandler(&observe.Server{}, pipe, db, func() *provider.Embedder { return nil }, nil)
	prints := []map[string]any{}
	for _, text := range []string{"current input", "current response"} {
		n, h := ingest.Fingerprint(text)
		prints = append(prints, map[string]any{"len": n, "h": h})
	}
	bodyBytes, _ := json.Marshal(map[string]any{"chat_id": "pdf-chat", "input": "Rina", "payload": prints})
	body := string(bodyBytes)
	code, baseline, raw := callModels(t, inject, "POST", body)
	if code != 200 || baseline["block"] == "" {
		t.Fatal(code, raw)
	}
	if _, present := baseline["transport"]; present {
		t.Fatal("default attached PDF")
	}
	if err = db.SetMemorySettings(2000, 4000, "auto", "", "google_pdf"); err != nil {
		t.Fatal(err)
	}
	code, pdf, raw := callModels(t, inject, "POST", body)
	if code != 200 {
		t.Fatal(code, raw)
	}
	tr, ok := pdf["transport"].(map[string]any)
	if !ok || tr["status"] != "ready" || tr["text"] != baseline["block"] {
		t.Fatal("missing exact selected PDF", raw)
	}
	if baseline["chars"].(float64) <= 0 || baseline["secret_chars"].(float64) <= 0 {
		t.Fatal("fixture must select both lanes", baseline)
	}
	pdfBytes, err := base64.StdEncoding.DecodeString(tr["pdf_base64"].(string))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := pdfreader.NewReader(bytes.NewReader(pdfBytes), int64(len(pdfBytes)))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := reader.GetPlainText()
	if err != nil {
		t.Fatal(err)
	}
	extracted, err := io.ReadAll(plain)
	if err != nil {
		t.Fatal(err)
	}
	flat := strings.ReplaceAll(string(extracted), "\n", "")
	if !strings.Contains(flat, "Rina is a pharmacist") || strings.Contains(flat, "princess") || strings.Contains(flat, "hide from") {
		t.Fatal("PDF lane leak/loss", flat)
	}
	retained := tr["retained_text"].(string)
	if !strings.Contains(retained, "Rina is a princess after royal confirmation") || !strings.Contains(retained, "hide from: Doyun") || strings.Contains(retained, "pharmacist") {
		t.Fatal("retained secret lost", retained)
	}
	if tr["pdf_sha256"] != fmt.Sprintf("%x", sha256.Sum256(pdfBytes)) || tr["pdf_bytes"] != float64(len(pdfBytes)) || tr["base64_chars"] != float64(len(tr["pdf_base64"].(string))) || tr["page_count"] != float64(reader.NumPage()) {
		t.Fatal("incorrect PDF metadata")
	}
	if err = db.SetMemorySettings(2000, 4000, "auto", "", "provider_manager_pdf"); err != nil {
		t.Fatal(err)
	}
	delegatedCode, delegated, delegatedRaw := callModels(t, inject, "POST", body)
	delegatedTransport, ok := delegated["transport"].(map[string]any)
	if delegatedCode != 200 || !ok || delegatedTransport["status"] != "delegated" || delegatedTransport["text"] != baseline["block"] {
		t.Fatal(delegatedRaw)
	}
	if _, present := delegatedTransport["pdf_base64"]; present {
		t.Fatal("delegation generated PDF")
	}
	delete(delegated, "transport")
	// Each request has its own diagnostic record even when transport alone
	// changes. Check that link before comparing the unchanged delivery fields.
	seen := map[float64]bool{}
	for _, response := range []map[string]any{baseline, pdf, delegated} {
		id, ok := response["delivery_id"].(float64)
		if !ok || id <= 0 || seen[id] {
			t.Fatal("missing or collapsed request trace", response)
		}
		seen[id] = true
		trace, err := db.DeliveryTraceRows("chat:pdf-chat", int64(id), 0, 100)
		if err != nil || trace == nil || trace.Block != response["block"] {
			t.Fatalf("request trace not linked: %+v %v", trace, err)
		}
		delete(response, "delivery_id")
	}
	// timings differ between requests (stages: 1.1 A1)
	for _, m := range []map[string]any{delegated, pdf, baseline} {
		delete(m, "total_ms")
		delete(m, "embed_ms")
		delete(m, "stages")
	}
	delete(pdf, "transport")
	if !reflect.DeepEqual(pdf, baseline) || !reflect.DeepEqual(delegated, baseline) {
		t.Fatalf("transport changed delivery: %#v vs %#v", pdf, baseline)
	}
}

func TestPDFTransportProviderManagerDelegatesWithoutGenerating(t *testing.T) {
	// This input is rejected by the real generator. Delegation must preserve it.
	block := "selected\x00text"
	tr := selectedMemoryTransport("provider_manager_pdf", block, nil)
	if tr == nil || tr.Status != "delegated" || tr.Text != block || tr.PDFBase64 != "" || tr.Error != "" {
		t.Fatalf("delegation generated or changed text: %+v", tr)
	}
}

func TestPDFTransportEmptyProviderManagerExplicitDelegation(t *testing.T) {
	tr := selectedMemoryTransport("provider_manager_pdf", "", nil)
	if tr == nil || tr.Mode != "provider_manager_pdf" || tr.Status != "delegated" || tr.Text != "" || tr.PDFBase64 != "" || tr.Error != "" {
		t.Fatalf("empty delegation: %+v", tr)
	}
	for _, mode := range []string{"text", "google_pdf"} {
		if tr := selectedMemoryTransport(mode, "", nil); tr != nil {
			t.Fatalf("empty %s must omit transport: %+v", mode, tr)
		}
	}
}

func TestPDFTransportSettingsErrorPreservesModeAndText(t *testing.T) {
	for _, mode := range []string{"text", "google_pdf", "provider_manager_pdf"} {
		for _, block := range []string{"", "selected\x00text"} {
			tr := selectedMemoryTransport(mode, block, errors.New("synthetic settings failure"))
			if tr == nil || tr.Mode != mode || tr.Status != "failed" || tr.Text != block || tr.PDFBase64 != "" || tr.Error != "transport_settings_failed" {
				t.Fatalf("settings failure: %+v", tr)
			}
		}
	}
}

func TestPDFTransportReuseExactNormalText(t *testing.T) {
	calls := 0
	b := &memoryTransportBuilder{generate: func(text string) (pdfmemory.Document, error) { calls++; return pdfmemory.Generate(text) }}
	d := store.Delivery{Block: "normal + secret first", PDFBlock: "normal", RetainedBlock: "secret first"}
	first := b.selected("google_pdf", d, nil)
	same := b.selected("google_pdf", d, nil)
	d.Block, d.RetainedBlock = "normal + secret changed", "secret changed"
	changedSecret := b.selected("google_pdf", d, nil)
	if first.CacheHit || !same.CacheHit || !changedSecret.CacheHit || calls != 1 || same.PDFBase64 != first.PDFBase64 || changedSecret.PDFBase64 != first.PDFBase64 || changedSecret.OriginalSHA256 == first.OriginalSHA256 || changedSecret.RetainedText != d.RetainedBlock {
		t.Fatal("reuse crossed source/text ownership", calls)
	}
	for _, tr := range []*memoryTransport{first, same, changedSecret} {
		raw, err := base64.StdEncoding.DecodeString(tr.PDFBase64)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := pdfreader.NewReader(bytes.NewReader(raw), int64(len(raw)))
		if err != nil {
			t.Fatal(err)
		}
		if tr.PDFSHA256 != fmt.Sprintf("%x", sha256.Sum256(raw)) || tr.PDFBytes != len(raw) || tr.Base64Chars != len(tr.PDFBase64) || tr.PageCount != reader.NumPage() || tr.OriginalSHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(tr.Text))) || tr.PDFTextSHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(tr.PDFText))) {
			t.Fatal("cached/generated metadata mismatch")
		}
	}
	d.PDFBlock = "changed normal"
	if b.selected("google_pdf", d, nil).CacheHit {
		t.Fatal("changed normal is cache hit")
	}
	if calls != 2 {
		t.Fatal("changed normal reused stale PDF", calls)
	}
}

func TestPDFTransportSecretOnlyAndFailedNormal(t *testing.T) {
	d := store.Delivery{Block: "original secret", RetainedBlock: "original secret"}
	tr := selectedDeliveryTransport("google_pdf", d, nil)
	if tr.Status != "text_only" || tr.PDFBase64 != "" || tr.RetainedText != d.Block || tr.Text != d.Block || tr.PDFText != "" {
		t.Fatalf("secret-only: %+v", tr)
	}
	tr = selectedDeliveryTransport("provider_manager_pdf", d, nil)
	if tr.Status != "delegated" || tr.PDFText != "" || tr.RetainedText != d.Block {
		t.Fatalf("empty manual range: %+v", tr)
	}
	d.PDFBlock = "bad\x00normal"
	tr = selectedDeliveryTransport("google_pdf", d, nil)
	if tr.Status != "failed" || tr.Text != d.Block || tr.PDFBase64 != "" || tr.PDFSHA256 != "" {
		t.Fatalf("conversion failed original: %+v", tr)
	}
}

// E11 llm_gateway_pdf (AC1's LLM Gateway PDF): stored as chosen and made
// into the same PDF as google_pdf; the plugin attaches it in the OpenAI form.
func TestPDFTransportLLMGatewayMode(t *testing.T) {
	if got := store.MemoryTransportMode("llm_gateway_pdf"); got != "llm_gateway_pdf" {
		t.Fatalf("mode kept: %q", got)
	}
	d := store.Delivery{Block: "normal + secret", PDFBlock: "normal", RetainedBlock: "secret"}
	gw := selectedDeliveryTransport("llm_gateway_pdf", d, nil)
	g := selectedDeliveryTransport("google_pdf", d, nil)
	if gw == nil || gw.Status != "ready" || gw.Mode != "llm_gateway_pdf" || gw.PDFBase64 == "" || gw.PDFSHA256 != g.PDFSHA256 || gw.RetainedText != "secret" {
		t.Fatalf("gateway transport %+v", gw)
	}
	if tr := selectedDeliveryTransport("llm_gateway_pdf", store.Delivery{Block: "secret only", RetainedBlock: "secret only"}, nil); tr == nil || tr.Status != "text_only" {
		t.Fatalf("secret only: %+v", tr)
	}
	db := modelsDB(t)
	h := settingsHandler(db)
	if code, v, raw := callModels(t, h, "POST", `{"transport_mode":"llm_gateway_pdf"}`); code != 200 || v["transport_mode"] != "llm_gateway_pdf" {
		t.Fatal(code, raw)
	}
}
