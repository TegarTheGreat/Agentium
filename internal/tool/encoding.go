package tool

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Text encodings read and edit handle besides UTF-8: files keep theirs.
const (
	encUTF8    = ""
	encUTF16LE = "UTF-16LE"
	encUTF16BE = "UTF-16BE"
	encLatin1  = "Latin-1"
)

// decodeText returns b as UTF-8 text and the encoding it was in: UTF-16
// with a byte-order mark, or Latin-1 when it is not valid UTF-8 (the
// usual case for legacy text: every byte is a character). ok is false
// for binary data.
func decodeText(b []byte) (text, enc string, ok bool) {
	switch {
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}) && len(b)%2 == 0:
		return decode16(b[2:], binary.LittleEndian), encUTF16LE, true
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}) && len(b)%2 == 0:
		return decode16(b[2:], binary.BigEndian), encUTF16BE, true
	}
	if bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
		return "", "", false
	}
	if utf8.Valid(b) {
		return string(b), encUTF8, true
	}
	// Mostly control bytes is binary, not legacy text.
	ctl := 0
	for _, c := range b[:min(len(b), 8000)] {
		if c < 0x20 && c != '\n' && c != '\r' && c != '\t' && c != '\f' {
			ctl++
		}
	}
	if ctl*20 > min(len(b), 8000) {
		return "", "", false
	}
	var sb strings.Builder
	for _, c := range b {
		sb.WriteRune(rune(c))
	}
	return sb.String(), encLatin1, true
}

func decode16(b []byte, order binary.ByteOrder) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = order.Uint16(b[2*i:])
	}
	return string(utf16.Decode(u))
}

// encodeText turns UTF-8 text back into enc.
func encodeText(s, enc string) ([]byte, error) {
	switch enc {
	case encUTF16LE, encUTF16BE:
		var order binary.AppendByteOrder = binary.LittleEndian
		out := []byte{0xFF, 0xFE}
		if enc == encUTF16BE {
			order, out = binary.BigEndian, []byte{0xFE, 0xFF}
		}
		for _, u := range utf16.Encode([]rune(s)) {
			out = order.AppendUint16(out, u)
		}
		return out, nil
	case encLatin1:
		out := make([]byte, 0, len(s))
		for _, r := range s {
			if r > 0xFF {
				return nil, fmt.Errorf("the file is %s and cannot hold %q; use a Latin-1 character or an escape", encLatin1, r)
			}
			out = append(out, byte(r))
		}
		return out, nil
	}
	return []byte(s), nil
}
