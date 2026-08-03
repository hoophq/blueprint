package render

import (
	"bufio"
	"compress/gzip"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hoophq/blueprint/internal/model"
)

// reportTemplate is the single-file report shell. It contains all CSS/JS
// inline and exactly two injection points (metaMarker, dataMarker), so the
// report is self-contained and renders fully offline.
//
//go:embed report.html.tmpl
var reportTemplate string

const (
	// metaMarker takes the headline block: scan metadata, the failure ledger,
	// the cost rollups, and the pre-aggregated summary. Always plain JSON.
	metaMarker = "__BLUEPRINT_META__"
	// dataMarker takes the census block: every resource, transposed, and either
	// plain JSON or gzip+base64 depending on how many there are.
	dataMarker = "__BLUEPRINT_DATA__"
)

// compressAbove is the resource count past which the census block is gzipped
// and base64'd.
//
// A count, not a byte size, because the cost is paid in the browser and the
// browser's cost scales with rows. Below the threshold the block stays plain
// text: the file is already small, the page decodes it synchronously with no
// DecompressionStream dependency, and anyone poking at the artifact with grep
// or a text editor can still read it. Above it, gzip is the difference between
// roughly 400 bytes a resource and roughly 55 — tens of megabytes against a
// couple, once there are enough rows to mean it — and that is worth an async
// decode and a floor on browser age. The measured figures live in one place,
// the table on budgetBytesPerResource in budget_test.go; this comment rounds
// them rather than restating them, so there is nothing here to drift.
//
// 5,000 is chosen where the tradeoff flips rather than measured to a point: a
// few thousand rows is a couple of megabytes, which is large enough to be
// annoying and small enough not to matter, so either answer is defensible and
// keeping small reports legible wins.
const compressAbove = 5000

// HTML writes the browsable map: a fully self-contained single-file report
// (data embedded, no external assets, renders offline).
//
// The page is streamed rather than assembled. At census scale the old
// build-one-big-string approach held the template, the marshalled snapshot and
// the joined page in memory at once — three copies of something that can be
// tens of megabytes — to produce a file that is written straight to disk and
// never inspected. Here the template's fixed parts go out as they are read and
// the two data blocks are encoded directly into the file.
//
// The write is atomic with respect to the destination: everything goes to
// path+".tmp", which is renamed onto path only after the last byte is flushed
// and the file is closed, so a mid-write failure never leaves a half-written
// report where a whole one used to be.
func HTML(snap *model.Snapshot, path string) error {
	prefix, middle, suffix, err := splitTemplate()
	if err != nil {
		return err
	}

	encoding := encodingJSON
	if len(snap.Resources) > compressAbove {
		encoding = encodingGzip
	}
	table, err := buildTable(snap.Resources)
	if err != nil {
		return err
	}
	meta := newReportMeta(snap, encoding)

	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	// On error paths the deferred calls discard the partial temp file. On the
	// success path the file is already closed and renamed away, so both are
	// no-ops. Defer order (LIFO): close first, then remove.
	defer os.Remove(tmp)
	defer f.Close()

	buf := bufio.NewWriterSize(f, 1<<16)
	// The template's own markup goes out verbatim; only the two data blocks are
	// written through the escaper, so no encoder in the chain can put a
	// script-data-ending sequence into the page even if its own escaping
	// changes. See htmlSafeWriter.
	data := &htmlSafeWriter{w: buf}

	if _, err := io.WriteString(buf, prefix); err != nil {
		return err
	}
	if err := json.NewEncoder(data).Encode(meta); err != nil {
		return err
	}
	if _, err := io.WriteString(buf, middle); err != nil {
		return err
	}
	if err := writeTable(data, table, encoding); err != nil {
		return err
	}
	if _, err := io.WriteString(buf, suffix); err != nil {
		return err
	}

	if err := buf.Flush(); err != nil {
		return err
	}
	// Close before rename so buffered data is durably in the temp file and
	// close errors are surfaced instead of silently renaming a bad file.
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// splitTemplate cuts the shell at its two injection points.
//
// Both must be present and in order. A template missing one, or carrying them
// the wrong way round, is a corrupt embed rather than a runtime condition, so
// this reports it instead of writing a report that would silently lose half
// its data.
func splitTemplate() (prefix, middle, suffix string, err error) {
	prefix, rest, ok := strings.Cut(reportTemplate, metaMarker)
	if !ok {
		return "", "", "", fmt.Errorf("report template is missing the meta marker %q; the embedded template is corrupt", metaMarker)
	}
	middle, suffix, ok = strings.Cut(rest, dataMarker)
	if !ok {
		return "", "", "", fmt.Errorf("report template is missing the data marker %q; the embedded template is corrupt", dataMarker)
	}
	return prefix, middle, suffix, nil
}

// writeTable encodes the census block into the page.
//
// The compressed path is a chain: JSON into gzip into base64 into the file, so
// the full census is never materialised as bytes anywhere. Both writers have
// to be closed in order — gzip to flush its trailer, then base64 to emit its
// padding — before the page can continue past the block.
func writeTable(w io.Writer, table resourceTable, encoding string) error {
	switch encoding {
	case encodingJSON:
		return json.NewEncoder(w).Encode(table)
	case encodingGzip:
		b64 := base64.NewEncoder(base64.StdEncoding, w)
		// BestCompression: the report is written once and opened many times,
		// and the extra CPU is a rounding error beside the AWS calls that
		// produced the snapshot. Header.ModTime is left zero, which is what
		// keeps the artifact byte-for-byte reproducible for a given snapshot.
		gz, err := gzip.NewWriterLevel(b64, gzip.BestCompression)
		if err != nil {
			return err
		}
		if err := json.NewEncoder(gz).Encode(table); err != nil {
			return err
		}
		if err := gz.Close(); err != nil {
			return err
		}
		return b64.Close()
	default:
		return fmt.Errorf("unknown census block encoding %q", encoding)
	}
}

// htmlSafeWriter passes bytes through, rewriting every '<' as the equivalent
// JSON escape.
//
// The blocks it wraps are JSON and base64. Base64's alphabet has no '<' at
// all, and in JSON a literal '<' can only appear inside a string — the
// structure itself (delimiters, numbers, literals) never contains one — so
// substituting the escape is both safe and total. That makes it impossible for
// an embedded block to contain "</script", "<!--" or "<script", the sequences
// that end the HTML parser's script-data state, whatever the encoders upstream
// decide to do about HTML escaping.
//
// A raw '<' is a single ASCII byte and UTF-8 never emits 0x3C as part of a
// multi-byte sequence, so scanning bytes cannot split a rune or fire on one.
type htmlSafeWriter struct {
	w io.Writer
}

// jsonLessThan is '<' written as a JSON string escape.
const jsonLessThan = "\\u003c"

func (h *htmlSafeWriter) Write(p []byte) (int, error) {
	start := 0
	for i, b := range p {
		if b != '<' {
			continue
		}
		if _, err := h.w.Write(p[start:i]); err != nil {
			return 0, err
		}
		if _, err := io.WriteString(h.w, jsonLessThan); err != nil {
			return 0, err
		}
		start = i + 1
	}
	if _, err := h.w.Write(p[start:]); err != nil {
		return 0, err
	}
	// The contract is bytes consumed, not bytes emitted: the escape is longer
	// than what it replaces, and reporting that would look like a short write
	// to every caller in the chain.
	return len(p), nil
}
