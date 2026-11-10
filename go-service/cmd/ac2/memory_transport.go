package main

import (
	"archivecenter2/goservice/internal/store"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"sync"

	"archivecenter2/goservice/internal/pdfmemory"
)

type memoryTransport struct {
	Mode           string `json:"mode"`
	Status         string `json:"status"`
	Text           string `json:"text"`
	MIMEType       string `json:"mime_type"`
	PDFBase64      string `json:"pdf_base64,omitempty"`
	Error          string `json:"error,omitempty"`
	PDFText        string `json:"pdf_text"`
	RetainedText   string `json:"retained_text"`
	OriginalSHA256 string `json:"original_sha256"`
	PDFTextSHA256  string `json:"pdf_text_sha256"`
	PDFSHA256      string `json:"pdf_sha256,omitempty"`
	PageCount      int    `json:"page_count,omitempty"`
	PDFBytes       int    `json:"pdf_bytes,omitempty"`
	Base64Chars    int    `json:"base64_chars,omitempty"`
	CacheHit       bool   `json:"cache_hit"`
}

// Conversion consumes the already selected final block; it cannot alter delivery.
func selectedMemoryTransport(mode, block string, settingsErr error) *memoryTransport {
	return selectedDeliveryTransport(mode, store.Delivery{Block: block, PDFBlock: block}, settingsErr)
}

type memoryTransportBuilder struct {
	mu       sync.Mutex
	text     string
	doc      pdfmemory.Document
	generate func(string) (pdfmemory.Document, error)
}

func selectedDeliveryTransport(mode string, d store.Delivery, settingsErr error) *memoryTransport {
	return (&memoryTransportBuilder{}).selected(mode, d, settingsErr)
}

func (b *memoryTransportBuilder) selected(mode string, d store.Delivery, settingsErr error) *memoryTransport {
	block := d.Block
	t := &memoryTransport{Mode: mode, Status: "failed", Text: block, PDFText: d.PDFBlock, RetainedText: d.RetainedBlock, MIMEType: "application/pdf", OriginalSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(block))), PDFTextSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(d.PDFBlock)))}
	if settingsErr != nil {
		log.Printf("memory transport: settings: %v", settingsErr)
		t.Error = "transport_settings_failed"
		return t
	}
	if mode == "provider_manager_pdf" {
		t.Status = "delegated"
		return t
	}
	if block == "" {
		return nil
	}
	// the PDF is made the same way for both; the plugin attaches it in the
	// request's own form (Google inlineData, or an OpenAI-compatible file part)
	if mode != "google_pdf" && mode != "llm_gateway_pdf" {
		return nil
	}
	if d.PDFBlock == "" {
		t.Status = "text_only"
		return t
	}
	b.mu.Lock()
	doc := b.doc
	t.CacheHit = b.text == d.PDFBlock && len(doc.Bytes) > 0
	var err error
	if b.text != d.PDFBlock || len(doc.Bytes) == 0 {
		generate := b.generate
		if generate == nil {
			generate = pdfmemory.Generate
		}
		doc, err = generate(d.PDFBlock)
		if err == nil {
			b.text, b.doc = d.PDFBlock, doc
		}
	}
	b.mu.Unlock()
	if err != nil {
		t.Error = "pdf_render_failed"
		switch {
		case errors.Is(err, pdfmemory.ErrInvalidUTF8):
			t.Error = "pdf_invalid_utf8"
		case errors.Is(err, pdfmemory.ErrUnsupportedText):
			t.Error = "pdf_unsupported_text"
		case errors.Is(err, pdfmemory.ErrFontLoad):
			t.Error = "pdf_font_failed"
		}
		log.Printf("memory transport: %s: %v", t.Error, err)
		return t
	}
	t.Status = "ready"
	t.PDFBase64 = base64.StdEncoding.EncodeToString(doc.Bytes)
	t.PDFSHA256 = fmt.Sprintf("%x", sha256.Sum256(doc.Bytes))
	t.PageCount, t.PDFBytes, t.Base64Chars = doc.PageCount, len(doc.Bytes), len(t.PDFBase64)
	return t
}
