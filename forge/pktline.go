package main

import (
	"fmt"
	"strconv"
)

const (
	PktLineMax        = 65520
	PktLineMaxPayload = PktLineMax - 4
	SidebandChunkMax  = 65515
	ZeroOID           = "0000000000000000000000000000000000000000"
)

var (
	PktFlushBytes       = []byte("0000")
	PktDelimBytes       = []byte("0001")
	PktResponseEndBytes = []byte("0002")
)

func EncodePktLine(payload []byte) []byte {
	n := len(payload) + 4
	if n > PktLineMax {
		panic("pkt-line payload too large")
	}
	hexHdr := fmt.Sprintf("%04x", n)
	out := make([]byte, n)
	copy(out[0:4], hexHdr)
	copy(out[4:], payload)
	return out
}

func EncodePktString(s string) []byte {
	return EncodePktLine([]byte(s))
}

func EncodeSideband1(data []byte) []byte {
	n := len(data) + 1 + 4
	hexHdr := fmt.Sprintf("%04x", n)
	out := make([]byte, n)
	copy(out[0:4], hexHdr)
	out[4] = 1 // sideband 1 = pack data / normal payload
	copy(out[5:], data)
	return out
}

func EncodeSideband1String(s string) []byte {
	return EncodeSideband1([]byte(s))
}

type PktPacket struct {
	Kind    string // "data", "flush", "delim", "response-end"
	Payload []byte
}

func DecodePktLines(data []byte) []PktPacket {
	var packets []PktPacket
	offset := 0
	for offset < len(data) {
		if offset+4 > len(data) {
			break
		}
		hexLen := string(data[offset : offset+4])
		l, err := strconv.ParseUint(hexLen, 16, 16)
		if err != nil {
			break
		}
		if l == 0 {
			packets = append(packets, PktPacket{Kind: "flush"})
			offset += 4
			continue
		}
		if l == 1 {
			packets = append(packets, PktPacket{Kind: "delim"})
			offset += 4
			continue
		}
		if l == 2 {
			packets = append(packets, PktPacket{Kind: "response-end"})
			offset += 4
			continue
		}
		if l < 4 || int(l) > len(data)-offset {
			break
		}
		payload := data[offset+4 : offset+int(l)]
		packets = append(packets, PktPacket{Kind: "data", Payload: payload})
		offset += int(l)
	}
	return packets
}

func PacketsToStringLines(packets []PktPacket) []string {
	var lines []string
	for _, p := range packets {
		if p.Kind == "data" {
			lines = append(lines, string(p.Payload))
		}
	}
	return lines
}
