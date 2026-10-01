package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
)

// Counting messages needs one field per jsonl line, but lines carrying tool output run to megabytes:
// decoding each into a map made `list --path` take 8s on a 156 MB directory. These read a top-level
// field by walking bytes, stopping at the key — no allocation, no dependence on key order.

// forEachLineBytes is forEachLine without the per-line string copy; line is only valid during the call.
func forEachLineBytes(path string, handle func(line []byte) bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for sc.Scan() {
		if !handle(sc.Bytes()) {
			return
		}
	}
}

// fieldStart returns the index of the first byte of key's value in a JSON object, ignoring nested
// objects and string contents. The object may be cut off after the key; anything past it is not read.
func fieldStart(obj []byte, key string) (int, bool) {
	depth, expectKey := 0, false
	for i := 0; i < len(obj); i++ {
		switch c := obj[i]; c {
		case '"':
			end := stringEnd(obj, i)
			if end < 0 {
				return 0, false
			}
			if depth == 1 && expectKey {
				expectKey = false
				if string(obj[i+1:end]) == key {
					j := skipSpace(obj, end+1)
					if j >= len(obj) || obj[j] != ':' {
						return 0, false
					}
					j = skipSpace(obj, j+1)
					return j, j < len(obj)
				}
			}
			i = end
		case '{', '[':
			depth++
			expectKey = c == '{' && depth == 1
		case '}', ']':
			if depth--; depth <= 0 {
				return 0, false
			}
		case ',':
			expectKey = depth == 1
		}
	}
	return 0, false
}

// topLevelString reads a top-level string field; false when absent or not a string.
func topLevelString(obj []byte, key string) (string, bool) {
	j, ok := fieldStart(obj, key)
	if !ok || obj[j] != '"' {
		return "", false
	}
	end := stringEnd(obj, j)
	if end < 0 {
		return "", false
	}
	raw := obj[j+1 : end]
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw), true
	}
	var s string
	if json.Unmarshal(obj[j:end+1], &s) != nil {
		return "", false
	}
	return s, true
}

// completeLine screens out a line still being written, which the full decode used to reject.
func completeLine(line []byte) bool {
	t := bytes.TrimRight(line, " \t\r")
	return len(t) > 0 && t[len(t)-1] == '}'
}

// valueEnd returns the index just past the JSON value starting at i, or -1 if it is cut off.
func valueEnd(b []byte, i int) int {
	if i >= len(b) {
		return -1
	}
	switch b[i] {
	case '"':
		if e := stringEnd(b, i); e >= 0 {
			return e + 1
		}
		return -1
	case '{', '[':
		depth := 0
		for k := i; k < len(b); k++ {
			switch b[k] {
			case '"':
				if k = stringEnd(b, k); k < 0 {
					return -1
				}
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return k + 1
				}
			}
		}
		return -1
	default: // number, true, false, null
		k := i
		for k < len(b) && !isDelim(b[k]) {
			k++
		}
		if k == i {
			return -1
		}
		return k
	}
}

// forEachElement calls handle with each element of the JSON array at the start of arr.
func forEachElement(arr []byte, handle func(elem []byte) bool) {
	if len(arr) == 0 || arr[0] != '[' {
		return
	}
	k := skipSpace(arr, 1)
	for k < len(arr) && arr[k] != ']' {
		e := valueEnd(arr, k)
		if e < 0 || !handle(arr[k:e]) {
			return
		}
		// Anything but a comma after an element ends the walk, so malformed input cannot loop.
		if k = skipSpace(arr, e); k >= len(arr) || arr[k] != ',' {
			return
		}
		k = skipSpace(arr, k+1)
	}
}

func isDelim(c byte) bool {
	switch c {
	case ',', '}', ']', ' ', '\t', '\r', '\n':
		return true
	}
	return false
}

// nonEmptyObject reports whether b starts with an object holding at least one member.
func nonEmptyObject(b []byte) bool {
	return len(b) > 0 && b[0] == '{' && skipSpace(b, 1) < len(b) && b[skipSpace(b, 1)] != '}'
}

// stringEnd returns the index of the quote closing the string opened at i, or -1.
func stringEnd(b []byte, i int) int {
	for k := i + 1; k < len(b); k++ {
		switch b[k] {
		case '\\':
			k++
		case '"':
			return k
		}
	}
	return -1
}

func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}
