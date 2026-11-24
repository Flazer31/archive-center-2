package pdfmemory

import (
	"debug/pe"
	"os"
	"syscall"
	"testing"
	"unsafe"
)

// The embedded font must live in a read-only section of the image: a
// writable section is charged as private memory from startup (11-24: about
// 10 MiB more than 1.4 before any PDF was made).
func TestEmbeddedFontIsReadOnlyImageData(t *testing.T) {
	font := embeddedFont()
	if len(font) == 0 {
		t.Fatal("no embedded font")
	}
	base, _, err := syscall.NewLazyDLL("kernel32.dll").NewProc("GetModuleHandleW").Call(0)
	if base == 0 {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f, err := pe.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rva := uintptr(unsafe.Pointer(&font[0])) - base
	for _, s := range f.Sections {
		if rva >= uintptr(s.VirtualAddress) && rva < uintptr(s.VirtualAddress)+uintptr(s.VirtualSize) {
			if s.Characteristics&pe.IMAGE_SCN_MEM_WRITE != 0 {
				t.Fatalf("embedded font is in writable section %s", s.Name)
			}
			return
		}
	}
	t.Fatalf("embedded font (RVA %#x) is not in the executable image", rva)
}
