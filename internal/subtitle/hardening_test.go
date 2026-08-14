package subtitle

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMarkupProjectionPreservesComparisonsAndUnknownTags(t *testing.T) {
	t.Parallel()
	input := "2 < 3 > 1; <unknown>x</unknown>; <b>bold</b> <font color=red>red</font> " +
		"<c.notice.urgent>classed</c> <v Roger>voice</v> <lang en-US>language</lang> " +
		"<ruby>ruby<rt>annotation</rt></ruby><br/>next &amp; final"
	got := projectMarkupText(input, true)
	want := "2 < 3 > 1; <unknown>x</unknown>; bold red classed voice language rubyannotation next & final"
	if got != want {
		t.Fatalf("projectMarkupText = %q\nwant %q", got, want)
	}

	for _, input := range []string{
		"literal <not-a-tag> text",
		"bad <c.> class",
		"bad <b:pseudo> tag",
		"unfinished <b",
		"nested-looking <not <b> text",
	} {
		if got := projectMarkupText(input, true); got != input {
			t.Errorf("projectMarkupText(%q) = %q; unknown/malformed markup must remain visible", input, got)
		}
	}
}

func TestVTTTimestampTagsAreFormatSpecific(t *testing.T) {
	t.Parallel()
	for _, timestamp := range []string{"00:01.250", "01:02:03.004", "99:59:59.999", "100:00:00.000"} {
		input := "before <" + timestamp + "> after"
		if got := projectMarkupText(input, true); got != "before  after" {
			t.Errorf("WebVTT projection of %q = %q", timestamp, got)
		}
		if got := projectMarkupText(input, false); got != input {
			t.Errorf("SRT projection of %q = %q; timestamp-looking text should remain visible", timestamp, got)
		}
	}
	for _, invalid := range []string{"0:01.250", "00:60.000", "00:01,250", " 00:01.250 ", "00:01.25"} {
		input := "before <" + invalid + "> after"
		if got := projectMarkupText(input, true); got != input {
			t.Errorf("invalid timestamp projection of %q = %q", invalid, got)
		}
	}

	vtt := mustParse(t, "timestamps.vtt", []byte("WEBVTT\n\n00:00.000 --> 00:03.000\nbefore <00:01.250> after\n"))
	if got := vtt.Cues[0].DisplayText; got != "before after" {
		t.Errorf("WebVTT display = %q", got)
	}
	srt := mustParse(t, "timestamps.srt", []byte("1\n00:00:00,000 --> 00:00:03,000\nbefore <00:01.250> after\n"))
	if got := srt.Cues[0].DisplayText; got != "before <00:01.250> after" {
		t.Errorf("SRT display = %q", got)
	}
}

func TestProjectionLargeMalformedDelimiters(t *testing.T) {
	t.Parallel()
	// A suffix-search implementation that advances one byte after each failed
	// search is quadratic on these inputs. One MiB makes that regression
	// conspicuous under the normal package test timeout without using a flaky
	// wall-clock assertion.
	const size = 1 << 20
	markup := strings.Repeat("<", size)
	if got := projectMarkupText(markup, true); got != markup {
		t.Fatalf("large malformed markup length/content changed: got %d bytes", len(got))
	}
	ass := strings.Repeat("{", size)
	if got := projectASSText(ass); got != ass {
		t.Fatalf("large malformed ASS length/content changed: got %d bytes", len(got))
	}
}

func TestASSUnmatchedOverrideDelimiterStaysVisible(t *testing.T) {
	t.Parallel()
	input := "ordinary { comparison \\N still visible"
	want := "ordinary { comparison   still visible"
	if got := projectASSText(input); got != want {
		t.Fatalf("projectASSText = %q, want %q", got, want)
	}
}

func TestUTF8OffsetMappingUsesNoTable(t *testing.T) {
	t.Parallel()
	raw := append([]byte{0xef, 0xbb, 0xbf}, bytes.Repeat([]byte("Café 😀\n"), 1<<12)...)
	view, encoding, err := decodeSource(FormatSRT, raw)
	if err != nil {
		t.Fatal(err)
	}
	if encoding != EncodingUTF8BOM || len(view.checkpoints) != 0 {
		t.Fatalf("encoding/checkpoints = %q/%d", encoding, len(view.checkpoints))
	}
	for offset := range view.text {
		got, err := view.rawOffset(offset)
		if err != nil || got != offset+3 {
			t.Fatalf("rawOffset(%d) = %d, %v; want %d", offset, got, err, offset+3)
		}
	}
	if got, err := view.rawOffset(len(view.text)); err != nil || got != len(raw) {
		t.Fatalf("EOF rawOffset = %d, %v; want %d", got, err, len(raw))
	}
	continuation := strings.Index(view.text, "é") + 1
	if _, err := view.rawOffset(continuation); err == nil {
		t.Fatalf("rawOffset accepted UTF-8 continuation byte at %d", continuation)
	}
}

func TestUTF16SparseOffsetMapping(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("ASCII", 3000) + "é" + strings.Repeat("界", 3000) + "😀" + strings.Repeat("z", 3000)
	raw := encodeUTF16Fixture(text, binary.LittleEndian, []byte{0xff, 0xfe})
	view, encoding, err := decodeSource(FormatSRT, raw)
	if err != nil {
		t.Fatal(err)
	}
	if encoding != EncodingUTF16LE {
		t.Fatalf("encoding = %q", encoding)
	}
	maxCheckpoints := len(view.text)/offsetCheckpointInterval + 2
	if len(view.checkpoints) > maxCheckpoints {
		t.Fatalf("got %d sparse checkpoints, want <= %d for %d decoded bytes", len(view.checkpoints), maxCheckpoints, len(view.text))
	}
	if len(view.checkpoints)*32 >= len(view.text) {
		t.Fatalf("checkpoint storage is unexpectedly dense: %d checkpoints for %d bytes", len(view.checkpoints), len(view.text))
	}

	wantRaw := 2
	runeIndex := 0
	for textOffset, r := range view.text {
		if runeIndex%137 == 0 {
			got, err := view.rawOffset(textOffset)
			if err != nil || got != wantRaw {
				t.Fatalf("rawOffset(%d) = %d, %v; want %d", textOffset, got, err, wantRaw)
			}
		}
		if r > 0xffff {
			wantRaw += 4
		} else {
			wantRaw += 2
		}
		runeIndex++
	}
	if got, err := view.rawOffset(len(view.text)); err != nil || got != len(raw) {
		t.Fatalf("EOF rawOffset = %d, %v; want %d", got, err, len(raw))
	}
	for offset := 1; offset < len(view.text); offset++ {
		if !utf8.RuneStart(view.text[offset]) {
			if _, err := view.rawOffset(offset); err == nil {
				t.Fatalf("rawOffset accepted continuation byte %d", offset)
			}
			break
		}
	}
}

func BenchmarkMalformedDelimiterProjection(b *testing.B) {
	for _, size := range []int{1 << 10, 1 << 16, 1 << 20} {
		b.Run("markup/"+benchmarkSize(size), func(b *testing.B) {
			input := strings.Repeat("<", size)
			b.SetBytes(int64(size))
			for range b.N {
				_ = projectMarkupText(input, true)
			}
		})
		b.Run("ass/"+benchmarkSize(size), func(b *testing.B) {
			input := strings.Repeat("{", size)
			b.SetBytes(int64(size))
			for range b.N {
				_ = projectASSText(input)
			}
		})
	}
}

func benchmarkSize(size int) string {
	switch size {
	case 1 << 10:
		return "1KiB"
	case 1 << 16:
		return "64KiB"
	default:
		return "1MiB"
	}
}
