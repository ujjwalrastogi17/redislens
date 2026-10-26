package rdb

import "errors"

// lzfDecompress decodes LZF data (used by Redis for compressed strings).
// Each control byte is either:
//
//	000LLLLL               -> literal run of L+1 bytes follows
//	LLLooooo [L2] oooooooo -> back-reference: copy len+2 bytes from offset back
func lzfDecompress(in []byte, outLen int) ([]byte, error) {
	out := make([]byte, 0, outLen)
	errCorrupt := errors.New("corrupt lzf data")
	i := 0
	for i < len(in) {
		ctrl := int(in[i])
		i++
		if ctrl < 32 {
			n := ctrl + 1
			if i+n > len(in) {
				return nil, errCorrupt
			}
			out = append(out, in[i:i+n]...)
			i += n
			continue
		}
		length := ctrl >> 5
		if length == 7 {
			if i >= len(in) {
				return nil, errCorrupt
			}
			length += int(in[i])
			i++
		}
		if i >= len(in) {
			return nil, errCorrupt
		}
		ref := len(out) - (ctrl&0x1F)<<8 - int(in[i]) - 1
		i++
		if ref < 0 {
			return nil, errCorrupt
		}
		// Byte-by-byte: source and destination may overlap (run-length style).
		for j := 0; j < length+2; j++ {
			out = append(out, out[ref+j])
		}
	}
	if len(out) != outLen {
		return nil, errCorrupt
	}
	return out, nil
}
