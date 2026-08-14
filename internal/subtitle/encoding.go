package subtitle

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

type sourceView struct {
	text     string
	encoding Encoding
	bomBytes int
	// UTF-8 sources map decoded offsets directly to raw offsets. UTF-16 uses
	// sparse checkpoints and scans about offsetCheckpointInterval decoded
	// bytes from the nearest checkpoint. This avoids an []int proportional to
	// every decoded byte (hundreds of MiB for a large subtitle file).
	checkpoints []offsetCheckpoint
}

type offsetCheckpoint struct {
	text int
	raw  int
}

// A 256-byte interval bounds translation work for cue-dense UTF-16 files while
// keeping the table near 6% of decoded text size (and zero for UTF-8). The old
// representation used one machine-sized integer for every decoded byte.
const offsetCheckpointInterval = 1 << 8

func (v sourceView) rawOffset(textOffset int) (int, error) {
	if textOffset < 0 || textOffset > len(v.text) ||
		(textOffset < len(v.text) && !utf8.RuneStart(v.text[textOffset])) {
		return 0, fmt.Errorf("invalid decoded-text boundary %d", textOffset)
	}
	if v.encoding == EncodingUTF8 || v.encoding == EncodingUTF8BOM {
		return v.bomBytes + textOffset, nil
	}
	if len(v.checkpoints) == 0 {
		return 0, errors.New("UTF-16 source has no offset checkpoints")
	}

	// Find the last checkpoint at or before textOffset.
	low, high := 0, len(v.checkpoints)
	for low < high {
		mid := low + (high-low)/2
		if v.checkpoints[mid].text <= textOffset {
			low = mid + 1
		} else {
			high = mid
		}
	}
	checkpoint := v.checkpoints[low-1]
	rawOffset := checkpoint.raw
	for _, r := range v.text[checkpoint.text:textOffset] {
		if r > 0xffff {
			rawOffset += 4
		} else {
			rawOffset += 2
		}
	}
	return rawOffset, nil
}

func (v sourceView) span(start, end int) (ByteSpan, error) {
	rawStart, err := v.rawOffset(start)
	if err != nil {
		return ByteSpan{}, err
	}
	rawEnd, err := v.rawOffset(end)
	if err != nil {
		return ByteSpan{}, err
	}
	return ByteSpan{Start: rawStart, End: rawEnd}, nil
}

func decodeSource(format Format, raw []byte) (sourceView, Encoding, error) {
	if len(raw) >= 2 && raw[0] == 0xff && raw[1] == 0xfe {
		if format == FormatWebVTT {
			return sourceView{}, "", errors.New("WebVTT must be UTF-8; UTF-16LE BOM found")
		}
		view, err := decodeUTF16(raw, binary.LittleEndian, EncodingUTF16LE)
		return view, EncodingUTF16LE, err
	}
	if len(raw) >= 2 && raw[0] == 0xfe && raw[1] == 0xff {
		if format == FormatWebVTT {
			return sourceView{}, "", errors.New("WebVTT must be UTF-8; UTF-16BE BOM found")
		}
		view, err := decodeUTF16(raw, binary.BigEndian, EncodingUTF16BE)
		return view, EncodingUTF16BE, err
	}

	bom := 0
	encoding := EncodingUTF8
	if len(raw) >= 3 && raw[0] == 0xef && raw[1] == 0xbb && raw[2] == 0xbf {
		bom = 3
		encoding = EncodingUTF8BOM
	}
	payload := raw[bom:]
	if !utf8.Valid(payload) {
		return sourceView{}, "", errors.New("malformed UTF-8")
	}
	view := sourceView{text: string(payload), encoding: encoding, bomBytes: bom}
	return view, encoding, nil
}

func decodeUTF16(raw []byte, order binary.ByteOrder, encoding Encoding) (sourceView, error) {
	if (len(raw)-2)%2 != 0 {
		return sourceView{}, errors.New("odd-length UTF-16 data")
	}
	var text strings.Builder
	// UTF-8 output never exceeds twice the UTF-16 payload for valid input.
	text.Grow(len(raw) - 2)
	checkpoints := []offsetCheckpoint{{text: 0, raw: 2}}
	lastCheckpointText := 0
	for pos := 2; pos < len(raw); {
		start := pos
		first := rune(order.Uint16(raw[pos : pos+2]))
		pos += 2
		var decoded rune
		switch {
		case 0xd800 <= first && first <= 0xdbff:
			if pos+2 > len(raw) {
				return sourceView{}, errors.New("unpaired high surrogate in UTF-16 data")
			}
			second := rune(order.Uint16(raw[pos : pos+2]))
			if second < 0xdc00 || second > 0xdfff {
				return sourceView{}, errors.New("unpaired high surrogate in UTF-16 data")
			}
			decoded = utf16.DecodeRune(first, second)
			pos += 2
		case 0xdc00 <= first && first <= 0xdfff:
			return sourceView{}, errors.New("unpaired low surrogate in UTF-16 data")
		default:
			decoded = first
		}

		before := text.Len()
		if before-lastCheckpointText >= offsetCheckpointInterval {
			checkpoints = append(checkpoints, offsetCheckpoint{text: before, raw: start})
			lastCheckpointText = before
		}
		text.WriteRune(decoded)
	}
	return sourceView{text: text.String(), encoding: encoding, bomBytes: 2, checkpoints: checkpoints}, nil
}

func encodeReplacement(text string, encoding Encoding) ([]byte, error) {
	switch encoding {
	case EncodingUTF8, EncodingUTF8BOM:
		return []byte(text), nil
	case EncodingUTF16LE, EncodingUTF16BE:
		units := utf16.Encode([]rune(text))
		out := make([]byte, len(units)*2)
		var order binary.ByteOrder = binary.LittleEndian
		if encoding == EncodingUTF16BE {
			order = binary.BigEndian
		}
		for i, unit := range units {
			order.PutUint16(out[i*2:], unit)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown encoding %q", encoding)
	}
}

type sourceLine struct {
	text       string
	start, end int // decoded offsets excluding the terminator
	fullEnd    int // decoded offset including the terminator
	number     int
}

func splitSourceLines(text string) []sourceLine {
	if text == "" {
		return nil
	}
	lines := make([]sourceLine, 0, strings.Count(text, "\n")+1)
	start := 0
	number := 1
	for start < len(text) {
		i := start
		for i < len(text) && text[i] != '\r' && text[i] != '\n' {
			i++
		}
		fullEnd := i
		if i < len(text) {
			if text[i] == '\r' && i+1 < len(text) && text[i+1] == '\n' {
				fullEnd = i + 2
			} else {
				fullEnd = i + 1
			}
		}
		lines = append(lines, sourceLine{
			text: text[start:i], start: start, end: i, fullEnd: fullEnd, number: number,
		})
		number++
		start = fullEnd
	}
	return lines
}
