package main

import "strconv"

// json.go — the JSON the REST surface writes, built by appending into one buffer.
//
// A reply is serialised exactly once and then held as bytes: the read workloads answer
// from a cached buffer, so nothing here runs on the hot path twice.

const jsonHex = "0123456789abcdef"

// appendJSONString appends s as a JSON string literal, escaping the bytes JSON requires.
func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		dst = append(dst, s[start:i]...)
		switch c {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		default:
			dst = append(dst, '\\', 'u', '0', '0', jsonHex[c>>4], jsonHex[c&0xf])
		}
		start = i + 1
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

// appendJSONInt appends an integer.
func appendJSONInt(dst []byte, value int64) []byte {
	return strconv.AppendInt(dst, value, 10)
}

// appendKey starts one object member: `,"name":`.
func appendKey(dst []byte, name string, first bool) []byte {
	if !first {
		dst = append(dst, ',')
	}
	dst = appendJSONString(dst, name)
	return append(dst, ':')
}

// appendJSONStringArray appends a list of strings.
func appendJSONStringArray(dst []byte, values []string) []byte {
	dst = append(dst, '[')
	for i, value := range values {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = appendJSONString(dst, value)
	}
	return append(dst, ']')
}
