// Command generate builds sounds.go from language folders with 0.wav..9.wav each.
//
// Example:
//   go run ./cmd/generate/generate.go -in . -out sounds.go -pkg captcha -langs en,es,ja,pt,ru,zh
//
// Input structure:
//   ./en/0.wav .. 9.wav
//   ./es/0.wav .. 9.wav
//   ...
//
// Output: a Go file that defines:
//   - var waveHeader = []byte{ ... }   // mono 8kHz 8-bit PCM RIFF header (sizes 0)
//   - var digitSounds = map[string][][]byte{ "en": { /* 0..9 */ }, "es": {...}, ... }

package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"go/format"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	inDir   = flag.String("in", ".", "Input directory containing language subfolders (en, es, ...)")
	outFile = flag.String("out", "sounds.go", "Output Go source file to generate")
	pkgName = flag.String("pkg", "captcha", "Package name for the generated Go file")
	langs   = flag.String("langs", "", "Comma-separated languages to include (default: auto-detect subfolders)")
	beep    = flag.String("beep", "", "Optional path to beep.wav (default: <in>/beep.wav). If missing, beepSound is omitted.")
)

// Target audio format for captcha: mono 8kHz unsigned 8-bit PCM (no header in digit bytes).
const (
	targetSR       = 8000
	targetBits     = 8
	targetChannels = 1
)

type wavInfo struct {
	SampleRate    int
	NumChannels   int
	BitsPerSample int
	AudioFormat   uint16 // 1=PCM
	Data          []byte // raw bytes as in file (interleaved)
}

// hard-coded header for RIFF/WAVE mono 8kHz 8-bit PCM, with sizes set to 0.
// This mirrors the original project approach.
var targetWaveHeader = []byte{
	0x52, 0x49, 0x46, 0x46, // "RIFF"
	0x00, 0x00, 0x00, 0x00, // size (filled elsewhere by writer)
	0x57, 0x41, 0x56, 0x45, // "WAVE"
	0x66, 0x6d, 0x74, 0x20, // "fmt "
	0x10, 0x00, 0x00, 0x00, // fmt chunk size (16)
	0x01, 0x00, // PCM
	0x01, 0x00, // channels = 1
	0x40, 0x1f, 0x00, 0x00, // sample rate = 8000 (0x1F40)
	0x40, 0x1f, 0x00, 0x00, // byte rate = 8000 (sr * ch * bits/8) -> 8000*1*1
	0x01, 0x00, // block align = ch * bits/8 = 1*1=1
	0x08, 0x00, // bits per sample = 8
	0x64, 0x61, 0x74, 0x61, // "data"
}

// Build map[lang][10][]byte
type langData struct {
	Lang   string
	Digits [10][]byte
}

func main() {
	flag.Parse()

	langsList := []string{}
	if *langs != "" {
		for _, l := range strings.Split(*langs, ",") {
			l = strings.TrimSpace(l)
			if l != "" {
				langsList = append(langsList, l)
			}
		}
	} else {
		// auto-detect: subfolders with digits 0.wav..9.wav
		entries, err := os.ReadDir(*inDir)
		check(err)
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if hasAllDigitFiles(filepath.Join(*inDir, name)) {
				langsList = append(langsList, name)
			}
		}
	}
	if len(langsList) == 0 {
		fatalf("no languages found. Provide -langs or ensure subfolders (en, es, ...) exist with 0.wav..9.wav")
	}
	sort.Strings(langsList)

	var all []langData

	for _, lang := range langsList {
		fmt.Printf(">> Processing lang %q\n", lang)
		var ld langData
		ld.Lang = lang
		for d := 0; d < 10; d++ {
			path := filepath.Join(*inDir, lang, fmt.Sprintf("%d.wav", d))
			u8, err := loadAsMonoU8(path)
			if err != nil {
				fatalf("lang=%s digit=%d: %v", lang, d, err)
			}
			ld.Digits[d] = u8
			fmt.Printf("   - %d.wav -> %d samples (8kHz u8)\n", d, len(u8))
		}
		all = append(all, ld)
	}

	// Beep (optional)
	var beepBytes []byte
	beepPath := *beep
	if beepPath == "" {
		beepPath = filepath.Join(*inDir, "beep.wav")
	}
	if _, err := os.Stat(beepPath); err == nil {
		u8, err := loadAsMonoU8(beepPath)
		check(err)
		beepBytes = u8
		fmt.Printf(">> Beep: %s -> %d samples (8kHz u8)\n", beepPath, len(beepBytes))
	} else {
		fmt.Printf(">> Beep: %s not found; skipping beepSound generation\n", beepPath)
	}

	src := generateSource(*pkgName, all, targetWaveHeader, beepBytes)
	formatted, err := format.Source([]byte(src))
	if err != nil {
		// si falla el formato, escribimos igual para facilitar debugging
		fmt.Fprintf(os.Stderr, "go/format error: %v\n", err)
		formatted = []byte(src)
	}
	check(os.WriteFile(*outFile, formatted, 0o644))
	fmt.Printf("OK. Wrote %s (%d bytes)\n", *outFile, len(formatted))
}

func hasAllDigitFiles(dir string) bool {
	for d := 0; d < 10; d++ {
		_, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%d.wav", d)))
		if err != nil {
			return false
		}
	}
	return true
}

func loadAsMonoU8(path string) ([]byte, error) {
	w, err := readWAV(path)
	if err != nil {
		return nil, fmt.Errorf("read wav: %w", err)
	}
	// decode to float mono
	fmono, sr, err := toFloatMono(w)
	if err != nil {
		return nil, fmt.Errorf("to float mono: %w", err)
	}
	// resample to 8kHz if needed
	if sr != targetSR {
		fmono = resampleLinear(fmono, sr, targetSR)
		sr = targetSR
	}
	// convert to unsigned 8-bit PCM
	u8 := floatToU8(fmono)
	return u8, nil
}

func readWAV(path string) (*wavInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Read RIFF header
	var hdr [12]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	if string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		return nil, fmt.Errorf("not a RIFF/WAVE file")
	}

	var (
		afmt          uint16
		channels      uint16
		sampleRate    uint32
		bitsPerSample uint16
		dataChunk     []byte
	)

	// iterate chunks
	for {
		var chdr [8]byte
		if _, err := io.ReadFull(f, chdr[:]); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("read chunk header: %w", err)
		}
		id := string(chdr[0:4])
		size := binary.LittleEndian.Uint32(chdr[4:8])

		switch id {
		case "fmt ":
			buf := make([]byte, size)
			if _, err := io.ReadFull(f, buf); err != nil {
				return nil, fmt.Errorf("read fmt: %w", err)
			}
			if size < 16 {
				return nil, fmt.Errorf("fmt chunk too small")
			}
			afmt = binary.LittleEndian.Uint16(buf[0:2])
			channels = binary.LittleEndian.Uint16(buf[2:4])
			sampleRate = binary.LittleEndian.Uint32(buf[4:8])
			// skip byteRate[8:12], blockAlign[12:14]
			bitsPerSample = binary.LittleEndian.Uint16(buf[14:16])
			// if extra fmt bytes, ignore
		case "data":
			dataChunk = make([]byte, size)
			if _, err := io.ReadFull(f, dataChunk); err != nil {
				return nil, fmt.Errorf("read data: %w", err)
			}
		default:
			// skip unknown chunk
			if _, err := f.Seek(int64(size), io.SeekCurrent); err != nil {
				return nil, fmt.Errorf("seek: %w", err)
			}
		}
		// stop if both parsed
		if afmt != 0 && dataChunk != nil && sampleRate != 0 && bitsPerSample != 0 && channels != 0 {
			// continue anyway, in case more chunks follow; data already read
			// but we can break to be safe
			// break
		}
		// WAV chunks are even-sized; if odd, skip pad byte
		if size%2 == 1 {
			if _, err := f.Seek(1, io.SeekCurrent); err != nil {
				return nil, fmt.Errorf("pad seek: %w", err)
			}
		}
		if dataChunk != nil && afmt != 0 && sampleRate != 0 {
			break
		}
	}

	if dataChunk == nil {
		return nil, fmt.Errorf("no data chunk")
	}
	if afmt != 1 { // PCM
		return nil, fmt.Errorf("unsupported audio format (want PCM=1), got %d", afmt)
	}
	if bitsPerSample != 8 && bitsPerSample != 16 {
		return nil, fmt.Errorf("unsupported bits per sample: %d (only 8 or 16)", bitsPerSample)
	}
	return &wavInfo{
		SampleRate:    int(sampleRate),
		NumChannels:   int(channels),
		BitsPerSample: int(bitsPerSample),
		AudioFormat:   afmt,
		Data:          dataChunk,
	}, nil
}

func toFloatMono(w *wavInfo) ([]float64, int, error) {
	// decode to float [-1,1], mixdown to mono if needed
	switch w.BitsPerSample {
	case 8:
		// unsigned 8-bit
		if w.NumChannels == 1 {
			n := len(w.Data)
			out := make([]float64, n)
			for i := 0; i < n; i++ {
				out[i] = (float64(uint8(w.Data[i]))/255.0)*2 - 1 // 0..255 -> -1..+1
			}
			return out, w.SampleRate, nil
		}
		// stereo: average
		if w.NumChannels == 2 {
			n := len(w.Data) / 2
			out := make([]float64, n)
			for i := 0; i < n; i++ {
				l := (float64(uint8(w.Data[2*i+0]))/255.0)*2 - 1
				r := (float64(uint8(w.Data[2*i+1]))/255.0)*2 - 1
				out[i] = 0.5 * (l + r)
			}
			return out, w.SampleRate, nil
		}
		return nil, 0, fmt.Errorf("unsupported channels=%d for 8-bit", w.NumChannels)

	case 16:
		// signed int16 little endian
		frameBytes := 2 * w.NumChannels
		if len(w.Data)%frameBytes != 0 {
			return nil, 0, fmt.Errorf("corrupt data length vs channels")
		}
		nFrames := len(w.Data) / frameBytes
		out := make([]float64, nFrames)
		if w.NumChannels == 1 {
			for i := 0; i < nFrames; i++ {
				v := int16(binary.LittleEndian.Uint16(w.Data[2*i : 2*i+2]))
				out[i] = float64(v) / 32768.0
			}
			return out, w.SampleRate, nil
		}
		if w.NumChannels == 2 {
			for i := 0; i < nFrames; i++ {
				l := int16(binary.LittleEndian.Uint16(w.Data[4*i : 4*i+2]))
				r := int16(binary.LittleEndian.Uint16(w.Data[4*i+2 : 4*i+4]))
				out[i] = 0.5 * (float64(l)/32768.0 + float64(r)/32768.0)
			}
			return out, w.SampleRate, nil
		}
		return nil, 0, fmt.Errorf("unsupported channels=%d for 16-bit", w.NumChannels)
	}
	return nil, 0, fmt.Errorf("unhandled bitsPerSample=%d", w.BitsPerSample)
}

func resampleLinear(x []float64, srFrom, srTo int) []float64 {
	if srFrom == srTo || len(x) == 0 {
		return append([]float64(nil), x...)
	}
	duration := float64(len(x)) / float64(srFrom)
	nTo := int(math.Round(duration * float64(srTo)))
	if nTo <= 0 {
		return []float64{}
	}
	out := make([]float64, nTo)
	ratio := float64(srFrom) / float64(srTo)
	for i := 0; i < nTo; i++ {
		// position in source
		pos := float64(i) * ratio
		idx := int(math.Floor(pos))
		frac := pos - float64(idx)
		if idx >= len(x)-1 {
			out[i] = x[len(x)-1]
			continue
		}
		out[i] = x[idx]*(1-frac) + x[idx+1]*frac
	}
	return out
}

func floatToU8(x []float64) []byte {
	out := make([]byte, len(x))
	for i, v := range x {
		if v > 1 {
			v = 1
		} else if v < -1 {
			v = -1
		}
		u := uint8(math.Round((v + 1) * 0.5 * 255.0)) // -1..1 -> 0..255
		out[i] = u
	}
	return out
}

func generateSource(pkg string, all []langData, waveHeader []byte, beep []byte) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by cmd/generate/generate.go; DO NOT EDIT.\n")
	fmt.Fprintf(&b, "// Generated at %s\n\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&b, "package %s\n\n", pkg)
	fmt.Fprintf(&b, "// This file has been generated from .wav files using generate.go.\n\n")
	// waveHeader
	fmt.Fprintf(&b, "var waveHeader = []byte{\n%s}\n\n", formatBytes(waveHeader, 12))
	fmt.Fprintf(&b, "// Byte slices contain raw 8 kHz unsigned 8-bit PCM data (without wav header).\n\n")
	fmt.Fprintf(&b, "var digitSounds = map[string][][]byte{\n")
	for _, ld := range all {
		fmt.Fprintf(&b, "\t%q: [][]byte{\n", ld.Lang)
		for d := 0; d < 10; d++ {
			fmt.Fprintf(&b, "\t\t{ // %d\n%s\t\t},\n", d, indent(formatBytes(ld.Digits[d], 11), 2))
		}
		fmt.Fprintf(&b, "\t},\n")
	}
	fmt.Fprintf(&b, "}\n")

	// beepSound (optional)
	if len(beep) > 0 {
		fmt.Fprintf(&b, "// beepSound contains raw 8 kHz unsigned 8-bit PCM (no WAV header), derived from beep.wav.\n")
		fmt.Fprintf(&b, "var beepSound = []byte{\n%s}\n", formatBytes(beep, 12))
	}

	return b.String()
}

func formatBytes(buf []byte, perLine int) string {
	var b strings.Builder
	for i, v := range buf {
		if i%perLine == 0 {
			b.WriteString("\t")
		}
		fmt.Fprintf(&b, "0x%02x", v)

		switch {
		case i == len(buf)-1:
			// Always put a comma after the last element (multiline requires it).
			b.WriteString(",\n")
		case (i+1)%perLine == 0:
			// End of line: comma + newline
			b.WriteString(",\n")
		default:
			// Between elements on the same line
			b.WriteString(", ")
		}
	}
	return b.String()
}

func indent(s string, tabs int) string {
	prefix := strings.Repeat("\t", tabs)
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		if len(ln) == 0 {
			continue
		}
		lines[i] = prefix + ln
	}
	return strings.Join(lines, "\n")
}

func check(err error) {
	if err != nil {
		fatalf("%v", err)
	}
}

func fatalf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "error: "+f+"\n", a...)
	os.Exit(1)
}
