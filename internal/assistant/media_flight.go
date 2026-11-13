package assistant

import (
	"math"
	"strconv"
	"strings"
)

// flight reads model rows and skips length-prefixed text by its exact UTF-8
// byte count. A malformed boundary stops parsing rather than searching inside
// arbitrary text for something that resembles another model row.
func (c *mediaCollector) flight(stream string) {
	for pos := 0; pos < len(stream) && len(c.audio) < 64 && c.nodes <= 16000; {
		if stream[pos] == '\n' || stream[pos] == '\r' {
			pos++
			continue
		}
		colon := pos
		for colon < len(stream) && flightHex(stream[colon]) {
			colon++
		}
		// Hint rows can have an empty ID, but all nonempty IDs are hex.
		if colon == len(stream) || stream[colon] != ':' {
			return
		}
		pos = colon + 1
		if pos == len(stream) {
			return
		}
		if stream[pos] == 'T' {
			start := pos + 1
			comma := start
			for comma < len(stream) && flightHex(stream[comma]) {
				comma++
			}
			if comma == start || comma == len(stream) || stream[comma] != ',' {
				return
			}
			size, err := strconv.ParseUint(stream[start:comma], 16, 64)
			pos = comma + 1
			if err != nil || size > math.MaxInt32 || size > uint64(len(stream)-pos) {
				return
			}
			pos += int(size)
			continue
		}
		end := len(stream)
		if newline := strings.IndexByte(stream[pos:], '\n'); newline >= 0 {
			end = pos + newline
		}
		// Tagged rows fail JSON decoding and are ignored as whole rows.
		c.json([]byte(stream[pos:end]))
		pos = end
	}
}

func flightHex(ch byte) bool {
	return ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f' || ch >= 'A' && ch <= 'F'
}
