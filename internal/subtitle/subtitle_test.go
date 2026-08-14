package subtitle

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"
)

func TestDetectFormat(t *testing.T) {
	t.Parallel()
	tests := map[string]Format{
		"movie.srt": FormatSRT, "MOVIE.SRT": FormatSRT,
		"captions.ass": FormatASS, "captions.AsS": FormatASS,
		"web.vtt": FormatWebVTT, "WEB.VTT": FormatWebVTT,
	}
	for path, want := range tests {
		got, err := DetectFormat(path)
		if err != nil || got != want {
			t.Errorf("DetectFormat(%q) = %q, %v; want %q", path, got, err, want)
		}
		if !SupportedExtension(path) {
			t.Errorf("SupportedExtension(%q) = false", path)
		}
	}
	for _, path := range []string{"captions.ssa", "captions.txt", "srt"} {
		if SupportedExtension(path) {
			t.Errorf("SupportedExtension(%q) = true", path)
		}
		if _, err := DetectFormat(path); !errors.Is(err, ErrUnsupportedFormat) {
			t.Errorf("DetectFormat(%q) error = %v; want ErrUnsupportedFormat", path, err)
		}
	}
}

func TestNormalizeAndLiteralSearch(t *testing.T) {
	t.Parallel()
	raw := []byte("1\n00:00:00,000 --> 00:00:01,000\nStraße  Cafe\u0301\tTHANKS [.*]\n")
	doc := mustParse(t, "sample.srt", raw)
	for _, query := range []string{"STRASSE", "café thanks", "[.*]", "  café\nthanks  "} {
		if got := len(doc.Search(query)); got != 1 {
			t.Errorf("Search(%q) matched %d cues, want 1", query, got)
		}
	}
	for _, query := range []string{"", " \t\n", "thanks .+", "missing"} {
		if got := len(doc.Search(query)); got != 0 {
			t.Errorf("Search(%q) matched %d cues, want 0", query, got)
		}
	}
}

func TestParseSRTSearchProjectionAndTimes(t *testing.T) {
	t.Parallel()
	raw := []byte("\xef\xbb\xbf001 \r\n00:00:01,250 --> 00:00:03.500 X1:2\r\n<i>Thank&nbsp;you</i><br>for &amp; watching\r\nsecond row\r\n\r\n2\n00:01:00,000 --> 00:01:01,001\nOther\n")
	doc := mustParse(t, "mixed.SRT", raw)
	if doc.Format != FormatSRT || doc.Encoding != EncodingUTF8BOM {
		t.Fatalf("format/encoding = %q/%q", doc.Format, doc.Encoding)
	}
	if len(doc.Cues) != 2 {
		t.Fatalf("got %d cues, want 2", len(doc.Cues))
	}
	first := doc.Cues[0]
	if first.Start != 1250*time.Millisecond || first.End != 3500*time.Millisecond {
		t.Errorf("first times = %v..%v", first.Start, first.End)
	}
	if want := "Thank you for & watching second row"; first.DisplayText != want {
		t.Errorf("DisplayText = %q, want %q", first.DisplayText, want)
	}
	if got := len(doc.Search("THANK YOU FOR & WATCHING")); got != 1 {
		t.Errorf("search matched %d cues, want 1", got)
	}
	if first.IndexSpan == nil || string(raw[first.IndexSpan.Start:first.IndexSpan.End]) != "001" {
		t.Errorf("unexpected SRT index span: %#v", first.IndexSpan)
	}
	if doc.Cues[1].Start != time.Minute || doc.Cues[1].End != time.Minute+1001*time.Millisecond {
		t.Errorf("second times = %v..%v", doc.Cues[1].Start, doc.Cues[1].End)
	}
}

func TestDeleteSRTRenumbersAndPreservesBytes(t *testing.T) {
	t.Parallel()
	raw := []byte("01  \r\n00:00:00,000 --> 00:00:01,000\r\none\r\n\r\n17\n00:00:02,000 --> 00:00:03,000\ntwo\n\n999\r00:00:04,000 --> 00:00:05,000\rthree")
	doc := mustParse(t, "sample.srt", raw)
	got, err := doc.Delete([]CueID{doc.Cues[1].ID})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("1  \r\n00:00:00,000 --> 00:00:01,000\r\none\r\n\r\n2\r00:00:04,000 --> 00:00:05,000\rthree")
	if !bytes.Equal(got, want) {
		t.Fatalf("delete output:\n%q\nwant:\n%q", got, want)
	}
	if err := doc.ValidateDeletion(got, []CueID{doc.Cues[1].ID}); err != nil {
		t.Fatalf("ValidateDeletion: %v", err)
	}

	noOp, err := doc.Delete(nil)
	if err != nil || !bytes.Equal(noOp, raw) {
		t.Fatalf("no-op = %q, %v; want exact original", noOp, err)
	}
	noOp[0] = 'X'
	if bytes.Equal(noOp, doc.OriginalBytes()) {
		t.Fatal("Delete or OriginalBytes did not return defensive data")
	}

	allIDs := cueIDs(doc.Cues)
	empty, err := doc.Delete(allIDs)
	if err != nil || len(empty) != 0 {
		t.Fatalf("delete all = %q, %v; want empty", empty, err)
	}
	if _, err := doc.Delete([]CueID{"not-a-cue"}); !errors.Is(err, ErrUnknownCue) {
		t.Fatalf("unknown cue error = %v", err)
	}
	if err := doc.ValidateDeletion(append(got, 'x'), []CueID{doc.Cues[1].ID}); err == nil {
		t.Fatal("ValidateDeletion accepted altered output")
	}
}

func TestDeleteAllSRTWithLeadingBlankLinesIsValidEmpty(t *testing.T) {
	t.Parallel()
	doc := mustParse(t, "sample.srt", []byte("\r\n\n1\n00:00:00,000 --> 00:00:01,000\ntext\n"))
	got, err := doc.Delete(cueIDs(doc.Cues))
	if err != nil || len(got) != 0 {
		t.Fatalf("delete all = %q, %v; want empty", got, err)
	}
	if err := doc.ValidateDeletion(got, cueIDs(doc.Cues)); err != nil {
		t.Fatal(err)
	}
}

func TestParseASSCustomFormatProjectionAndDelete(t *testing.T) {
	t.Parallel()
	raw := []byte("[Script Info]\r\nTitle: Keep exactly\r\n\r\n[Events]\r\nFormat: Layer, End, Start, Style, Text\r\nComment: 0,0:00:03.00,0:00:01.00,Default,Thank you for watching\r\nDialogue: 0,0:00:03.25,0:00:01.10,Default,{\\i1}Thank\\Nyou, friend{\\i0}\r\nDialogue: 0,0:00:05.00,0:00:04.00,Default,{\\p1}m 0 0 l 10 10{\\p0}Visible\\htext\r\n")
	doc := mustParse(t, "sample.ass", raw)
	if len(doc.Cues) != 2 {
		t.Fatalf("got %d cues, want 2", len(doc.Cues))
	}
	first := doc.Cues[0]
	if first.Start != 1100*time.Millisecond || first.End != 3250*time.Millisecond {
		t.Errorf("first times = %v..%v", first.Start, first.End)
	}
	if first.DisplayText != "Thank you, friend" {
		t.Errorf("first display = %q", first.DisplayText)
	}
	if doc.Cues[1].DisplayText != "Visible text" {
		t.Errorf("drawing projection = %q", doc.Cues[1].DisplayText)
	}
	if got := len(doc.Search("thank you, friend")); got != 1 {
		t.Errorf("search matched %d, want 1", got)
	}
	if got := len(doc.Search("m 0 0")); got != 0 {
		t.Errorf("drawing search matched %d, want 0", got)
	}

	got, err := doc.Delete([]CueID{first.ID})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("[Script Info]\r\nTitle: Keep exactly\r\n\r\n[Events]\r\nFormat: Layer, End, Start, Style, Text\r\nComment: 0,0:00:03.00,0:00:01.00,Default,Thank you for watching\r\nDialogue: 0,0:00:05.00,0:00:04.00,Default,{\\p1}m 0 0 l 10 10{\\p0}Visible\\htext\r\n")
	if !bytes.Equal(got, want) {
		t.Fatalf("delete output:\n%q\nwant:\n%q", got, want)
	}
	if err := doc.ValidateDeletion(got, []CueID{first.ID}); err != nil {
		t.Fatal(err)
	}
}

func TestParseWebVTTMetadataCueIDsAndDelete(t *testing.T) {
	t.Parallel()
	raw := []byte("WEBVTT Example\r\nLanguage: en\r\n\r\nNOTE generated by tool\r\nThank you for watching\r\n\r\nSTYLE\r\n::cue { color: lime }\r\n\r\nREGION\r\nid:fred\r\n\r\ncue-one\r\n00:01.000 --> 00:03.500 line:90%\r\n<v Roger><b>Thank</b><br>you &amp; all</v>\r\n\r\n00:00:04.000 --> 00:00:05.000\r\nOther\r\n")
	doc := mustParse(t, "sample.vtt", raw)
	if len(doc.Cues) != 2 {
		t.Fatalf("got %d cues, want 2", len(doc.Cues))
	}
	if doc.Cues[0].DisplayText != "Thank you & all" {
		t.Errorf("display = %q", doc.Cues[0].DisplayText)
	}
	if doc.Cues[0].Start != time.Second || doc.Cues[0].End != 3500*time.Millisecond {
		t.Errorf("times = %v..%v", doc.Cues[0].Start, doc.Cues[0].End)
	}
	if got := len(doc.Search("generated by tool")); got != 0 {
		t.Errorf("NOTE matched %d cues", got)
	}
	got, err := doc.Delete([]CueID{doc.Cues[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("WEBVTT Example\r\nLanguage: en\r\n\r\nNOTE generated by tool\r\nThank you for watching\r\n\r\nSTYLE\r\n::cue { color: lime }\r\n\r\nREGION\r\nid:fred\r\n\r\n00:00:04.000 --> 00:00:05.000\r\nOther\r\n")
	if !bytes.Equal(got, want) {
		t.Fatalf("delete output:\n%q\nwant:\n%q", got, want)
	}
}

func TestUTF16SRTAndASS(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		encoding Encoding
		order    binary.ByteOrder
		bom      []byte
	}{
		{name: "little", encoding: EncodingUTF16LE, order: binary.LittleEndian, bom: []byte{0xff, 0xfe}},
		{name: "big", encoding: EncodingUTF16BE, order: binary.BigEndian, bom: []byte{0xfe, 0xff}},
	} {
		t.Run(tc.name+"/srt", func(t *testing.T) {
			text := "4\r\n00:00:00,000 --> 00:00:01,000\r\nCafé 😀\r\n\r\n8\r\n00:00:02,000 --> 00:00:03,000\r\nDelete me\r\n"
			raw := encodeUTF16Fixture(text, tc.order, tc.bom)
			doc := mustParse(t, "sample.srt", raw)
			if doc.Encoding != tc.encoding || len(doc.Cues) != 2 {
				t.Fatalf("encoding/cues = %q/%d", doc.Encoding, len(doc.Cues))
			}
			if len(doc.Search("CAFÉ 😀")) != 1 {
				t.Fatal("Unicode UTF-16 search failed")
			}
			got, err := doc.Delete([]CueID{doc.Cues[1].ID})
			if err != nil {
				t.Fatal(err)
			}
			want := encodeUTF16Fixture("1\r\n00:00:00,000 --> 00:00:01,000\r\nCafé 😀\r\n\r\n", tc.order, tc.bom)
			if !bytes.Equal(got, want) {
				t.Fatalf("UTF-16 delete differs:\n%x\nwant:\n%x", got, want)
			}
		})

		t.Run(tc.name+"/ass", func(t *testing.T) {
			text := "[Events]\nFormat: Start, End, Text\nDialogue: 0:00:00.00,0:00:01.00,ありがとう\n"
			raw := encodeUTF16Fixture(text, tc.order, tc.bom)
			doc := mustParse(t, "sample.ass", raw)
			if doc.Encoding != tc.encoding || len(doc.Search("ありがとう")) != 1 {
				t.Fatalf("UTF-16 ASS parse/search failed: %q", doc.Encoding)
			}
		})
	}
}

func TestMalformedFilesAreRejected(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		path string
		raw  []byte
	}{
		{"srt missing id", "x.srt", []byte("00:00:00,000 --> 00:00:01,000\ntext\n")},
		{"srt missing timing", "x.srt", []byte("1\ntext\n")},
		{"srt empty payload", "x.srt", []byte("1\n00:00:00,000 --> 00:00:01,000\n\n")},
		{"srt bad time", "x.srt", []byte("1\n00:99:00,000 --> 00:00:01,000\ntext\n")},
		{"srt missing separator", "x.srt", []byte("1\n00:00:00,000 --> 00:00:01,000\ntext\n2\n00:00:02,000 --> 00:00:03,000\nmore\n")},
		{"srt overflowing time", "x.srt", []byte("1\n999999999999999999999999:00:00,000 --> 00:00:01,000\ntext\n")},
		{"srt duration overflow", "x.srt", []byte("1\n999999999:00:00,000 --> 999999999:00:01,000\ntext\n")},
		{"ass dialogue before format", "x.ass", []byte("[Events]\nDialogue: 0:00:00.00,0:00:01.00,text\n")},
		{"ass missing text field", "x.ass", []byte("[Events]\nFormat: Start, End\n")},
		{"ass text not last", "x.ass", []byte("[Events]\nFormat: Start, Text, End\n")},
		{"ass too few fields", "x.ass", []byte("[Events]\nFormat: Start, End, Text\nDialogue: 0:00:00.00,0:00:01.00\n")},
		{"vtt missing header", "x.vtt", []byte("00:00.000 --> 00:01.000\ntext\n")},
		{"vtt no header separator", "x.vtt", []byte("WEBVTT\nmetadata")},
		{"vtt empty payload", "x.vtt", []byte("WEBVTT\n\n00:00.000 --> 00:01.000\n")},
		{"vtt bad block", "x.vtt", []byte("WEBVTT\n\nnot a cue\n")},
		{"malformed UTF8", "x.srt", []byte{0xff}},
		{"odd UTF16", "x.srt", []byte{0xff, 0xfe, 0x31}},
		{"unpaired UTF16", "x.ass", []byte{0xff, 0xfe, 0x00, 0xd8}},
		{"UTF16 VTT", "x.vtt", encodeUTF16Fixture("WEBVTT\n", binary.LittleEndian, []byte{0xff, 0xfe})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.path, tc.raw)
			if err == nil {
				t.Fatal("Parse unexpectedly succeeded")
			}
			var parseErr *ParseError
			if !errors.As(err, &parseErr) {
				t.Fatalf("error %T %v is not ParseError", err, err)
			}
			if parseErr.Path != tc.path {
				t.Errorf("ParseError.Path = %q, want %q", parseErr.Path, tc.path)
			}
		})
	}
}

func TestZeroCueDocuments(t *testing.T) {
	t.Parallel()
	for path, raw := range map[string][]byte{
		"empty.srt": nil,
		"empty.ass": []byte("[Script Info]\nTitle: Empty\n\n[Events]\nFormat: Start, End, Text\n"),
		"empty.vtt": []byte("WEBVTT\n"),
	} {
		doc := mustParse(t, path, raw)
		if len(doc.Cues) != 0 {
			t.Errorf("%s: got %d cues", path, len(doc.Cues))
		}
		if got, err := doc.Delete(nil); err != nil || !bytes.Equal(got, raw) {
			t.Errorf("%s no-op = %q, %v", path, got, err)
		}
	}
}

func TestInputAndDocumentAreImmutable(t *testing.T) {
	t.Parallel()
	raw := []byte("1\n00:00:00,000 --> 00:00:01,000\nhello\n")
	want := bytes.Clone(raw)
	doc := mustParse(t, "x.srt", raw)
	raw[0] = '9'
	if !bytes.Equal(doc.OriginalBytes(), want) {
		t.Fatal("Parse retained caller's mutable input")
	}
	got := doc.Search("hello")
	got[0].DisplayText = "changed"
	if doc.Cues[0].DisplayText == "changed" {
		t.Fatal("Search returned aliased cue storage")
	}
	// The exported view is convenient to render, but safety-critical operations
	// use the immutable private index built by Parse.
	doc.Cues[0].DeleteSpan = ByteSpan{}
	if doc.Cues[0].IndexSpan != nil {
		doc.Cues[0].IndexSpan.Start = 0
		doc.Cues[0].IndexSpan.End = len(want)
	}
	searched := doc.Search("hello")
	if searched[0].IndexSpan != nil {
		searched[0].IndexSpan.Start = 0
		searched[0].IndexSpan.End = len(want)
	}
	doc.Format = FormatASS
	doc.Encoding = EncodingUTF16BE
	deleted, err := doc.Delete([]CueID{doc.Cues[0].ID})
	if err != nil || len(deleted) != 0 {
		t.Fatalf("mutating exported cue corrupted deletion: %q, %v", deleted, err)
	}
}

func TestDuplicateCueIDsInDeleteAreIdempotent(t *testing.T) {
	t.Parallel()
	doc := mustParse(t, "x.vtt", []byte("WEBVTT\n\n00:00.000 --> 00:01.000\ntext\n"))
	one, err := doc.Delete([]CueID{doc.Cues[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	two, err := doc.Delete([]CueID{doc.Cues[0].ID, doc.Cues[0].ID})
	if err != nil || !bytes.Equal(one, two) {
		t.Fatalf("duplicate deletion differs: %q / %q, %v", one, two, err)
	}
}

func TestParseFormatRejectsUnknown(t *testing.T) {
	t.Parallel()
	_, err := ParseFormat(Format("ssa"), nil)
	if !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("error = %v, want ErrUnsupportedFormat", err)
	}
}

func mustParse(t *testing.T, path string, raw []byte) *Document {
	t.Helper()
	doc, err := Parse(path, raw)
	if err != nil {
		t.Fatalf("Parse(%q): %v", path, err)
	}
	return doc
}

func cueIDs(cues []Cue) []CueID {
	ids := make([]CueID, len(cues))
	for i, cue := range cues {
		ids[i] = cue.ID
	}
	return ids
}

func encodeUTF16Fixture(text string, order binary.ByteOrder, bom []byte) []byte {
	units := utf16.Encode([]rune(text))
	out := append([]byte(nil), bom...)
	for _, unit := range units {
		pair := make([]byte, 2)
		order.PutUint16(pair, unit)
		out = append(out, pair...)
	}
	return out
}

func TestNormalizeQueryTable(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"  Foo\tBAR\n": "foo bar",
		"Straße":       "strasse",
		"Cafe\u0301":   "café",
		"\u2003\u00a0": "",
	}
	for input, want := range tests {
		if got := NormalizeQuery(input); got != want {
			t.Errorf("NormalizeQuery(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNormalizeQueryConcurrent(t *testing.T) {
	t.Parallel()
	const workers = 32
	const iterations = 100
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for range iterations {
				if got := NormalizeQuery("  Straße Cafe\u0301\t"); got != "strasse café" {
					t.Errorf("NormalizeQuery = %q", got)
					return
				}
			}
		}()
	}
	group.Wait()
}

func TestSearchOrderAndCueValueCopies(t *testing.T) {
	t.Parallel()
	doc := mustParse(t, "x.srt", []byte("1\n00:00:00,000 --> 00:00:01,000\nmatch\n\n2\n00:00:02,000 --> 00:00:03,000\nmatch match\n"))
	matches := doc.Search("match")
	if len(matches) != 2 {
		t.Fatalf("got %d matches", len(matches))
	}
	if !reflect.DeepEqual(cueIDs(matches), cueIDs(doc.Cues)) {
		t.Fatalf("search changed source order")
	}
	// A repeated occurrence is still one cue match.
	if strings.Count(doc.Cues[1].SearchText, "match") != 2 {
		t.Fatal("fixture is missing repeated occurrences")
	}
}
