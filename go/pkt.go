package main

// pkt.go — git's pkt-line framing: a 4-hex length (payload + 4) then the payload, with
// 0000 flush, 0001 delim and 0002 response-end as the control packets.

import (
	"errors"
)

const pktLinePayloadMax = 65516

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

// appendPktLine appends one data packet carrying payload.
func appendPktLine(dst, payload []byte) []byte {
	total := len(payload) + 4
	dst = append(dst,
		jsonHex[(total>>12)&0xf],
		jsonHex[(total>>8)&0xf],
		jsonHex[(total>>4)&0xf],
		jsonHex[total&0xf])
	return append(dst, payload...)
}

// appendPktLineString appends one data packet carrying text.
func appendPktLineString(dst []byte, text string) []byte {
	total := len(text) + 4
	dst = append(dst,
		jsonHex[(total>>12)&0xf],
		jsonHex[(total>>8)&0xf],
		jsonHex[(total>>4)&0xf],
		jsonHex[total&0xf])
	return append(dst, text...)
}

type packetKind int

const (
	packetData packetKind = iota
	packetFlush
	packetDelim
	packetResponseEnd
)

type packet struct {
	kind    packetKind
	payload []byte
	next    int
}

var errShortPacket = errors.New("packet is truncated")

// readPacketAt reads the packet at offset; ok is false when fewer than four bytes remain.
func readPacketAt(buf []byte, offset int) (packet, bool, error) {
	if offset+4 > len(buf) {
		return packet{}, false, nil
	}
	length := 0
	for _, c := range buf[offset : offset+4] {
		value := hexVal(c)
		if value < 0 {
			return packet{}, false, errors.New("invalid pkt-line length")
		}
		length = length<<4 | value
	}
	switch length {
	case 0:
		return packet{kind: packetFlush, next: offset + 4}, true, nil
	case 1:
		return packet{kind: packetDelim, next: offset + 4}, true, nil
	case 2:
		return packet{kind: packetResponseEnd, next: offset + 4}, true, nil
	}
	if length < 4 {
		return packet{}, false, errors.New("invalid pkt-line length")
	}
	if offset+length > len(buf) {
		return packet{}, false, errShortPacket
	}
	return packet{kind: packetData, payload: buf[offset+4 : offset+length], next: offset + length}, true, nil
}
