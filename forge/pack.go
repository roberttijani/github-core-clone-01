package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

type GitObjectType string

const (
	TypeCommit GitObjectType = "commit"
	TypeTree   GitObjectType = "tree"
	TypeBlob   GitObjectType = "blob"
	TypeTag    GitObjectType = "tag"
)

type GitObject struct {
	Type GitObjectType
	Data []byte
	OID  string
}

func HashObject(objType GitObjectType, data []byte) string {
	hdr := fmt.Sprintf("%s %d\x00", objType, len(data))
	h := sha1.New()
	h.Write([]byte(hdr))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

func ParseTree(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], ' ')
		if space < 0 {
			return nil, errors.New("tree entry has no mode separator")
		}
		space += offset
		nul := bytes.IndexByte(data[space+1:], 0)
		if nul < 0 {
			return nil, errors.New("tree entry has no name terminator")
		}
		nul += space + 1
		if nul+21 > len(data) {
			return nil, errors.New("tree entry has truncated oid")
		}
		mode := string(data[offset:space])
		name := string(data[space+1 : nul])
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
		offset = nul + 21
	}
	return entries, nil
}

func CompareTreeEntries(a, b TreeEntry) bool {
	aName := a.Name
	if a.Mode == "40000" {
		aName += "/"
	}
	bName := b.Name
	if b.Mode == "40000" {
		bName += "/"
	}
	return aName < bName
}

func BuildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		return CompareTreeEntries(sorted[i], sorted[j])
	})

	var buf bytes.Buffer
	for _, entry := range sorted {
		buf.WriteString(entry.Mode)
		buf.WriteByte(' ')
		buf.WriteString(entry.Name)
		buf.WriteByte(0)
		rawOid, _ := hex.DecodeString(entry.OID)
		buf.Write(rawOid)
	}
	return buf.Bytes()
}

type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func ParseCommit(data []byte) (CommitView, error) {
	var view CommitView
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
		view.Message = string(data[blank+2:])
	}
	headerText := string(data[:headerEnd])
	for _, line := range strings.Split(headerText, "\n") {
		if line == "" {
			continue
		}
		space := strings.IndexByte(line, ' ')
		var key, val string
		if space < 0 {
			key = line
		} else {
			key = line[:space]
			val = line[space+1:]
		}
		switch key {
		case "tree":
			view.Tree = val
		case "parent":
			view.Parents = append(view.Parents, val)
		case "author":
			view.Author = val
		case "committer":
			view.Committer = val
		}
	}
	return view, nil
}

func BuildCommit(tree string, parents []string, message string, author string) []byte {
	if author == "" {
		author = "ada iversen <adaiversen@example.invalid> 1700000000 +0000"
	}
	var lines []string
	lines = append(lines, "tree "+tree)
	for _, p := range parents {
		lines = append(lines, "parent "+p)
	}
	lines = append(lines, "author "+author)
	lines = append(lines, "committer "+author)
	lines = append(lines, "")
	body := strings.Join(lines, "\n") + "\n" + message
	return []byte(body)
}

func WritePackObjectHeader(objType GitObjectType, size int) []byte {
	var typeCode int
	switch objType {
	case TypeCommit:
		typeCode = 1
	case TypeTree:
		typeCode = 2
	case TypeBlob:
		typeCode = 3
	case TypeTag:
		typeCode = 4
	}
	var b []byte
	rem := size
	first := byte((typeCode << 4) | (rem & 0x0f))
	rem >>= 4
	for rem > 0 {
		b = append(b, first|0x80)
		first = byte(rem & 0x7f)
		rem >>= 7
	}
	b = append(b, first)
	return b
}

func WritePack(objects []GitObject) []byte {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(objects)))
	buf.Write(hdr)

	for _, obj := range objects {
		buf.Write(WritePackObjectHeader(obj.Type, len(obj.Data)))
		zw := zlib.NewWriter(&buf)
		zw.Write(obj.Data)
		zw.Close()
	}

	h := sha1.New()
	h.Write(buf.Bytes())
	trailer := h.Sum(nil)
	buf.Write(trailer)
	return buf.Bytes()
}

// Counting byte reader for zlib unpacking
type byteSliceReader struct {
	data []byte
	pos  int
}

func (r *byteSliceReader) ReadByte() (byte, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	b := r.data[r.pos]
	r.pos++
	return b, nil
}

func (r *byteSliceReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func readVarintSize(buf []byte, start int) (int, int, byte, error) {
	if start >= len(buf) {
		return 0, 0, 0, errors.New("pack entry truncated")
	}
	b := buf[start]
	first := b
	size := int(b & 0x0f)
	shift := uint(4)
	offset := start + 1
	for b&0x80 != 0 {
		if offset >= len(buf) {
			return 0, 0, 0, errors.New("pack entry varint truncated")
		}
		b = buf[offset]
		offset++
		size += int(b&0x7f) << shift
		shift += 7
	}
	return size, offset, first, nil
}

func readOfsDeltaBase(buf []byte, start int) (int, int, error) {
	if start >= len(buf) {
		return 0, 0, errors.New("ofs-delta truncated")
	}
	b := buf[start]
	offset := start + 1
	val := int(b & 0x7f)
	for b&0x80 != 0 {
		if offset >= len(buf) {
			return 0, 0, errors.New("ofs-delta truncated")
		}
		b = buf[offset]
		offset++
		val = ((val + 1) * 128) + int(b&0x7f)
	}
	return val, offset, nil
}

func ApplyDelta(base []byte, delta []byte) ([]byte, error) {
	offset := 0
	readSize := func() (int, error) {
		val := 0
		shift := uint(0)
		for {
			if offset >= len(delta) {
				return 0, errors.New("delta size varint truncated")
			}
			b := delta[offset]
			offset++
			val += int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return val, nil
	}
	sourceSize, err := readSize()
	if err != nil {
		return nil, err
	}
	targetSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if sourceSize != len(base) {
		return nil, fmt.Errorf("delta expects %d-byte base, got %d", sourceSize, len(base))
	}

	out := make([]byte, targetSize)
	written := 0
	for offset < len(delta) {
		op := delta[offset]
		offset++
		if op&0x80 != 0 {
			copyOffset := 0
			copySize := 0
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<bit) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("delta copy offset truncated")
					}
					copyOffset += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<bit) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("delta copy size truncated")
					}
					copySize += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOffset+copySize > len(base) || written+copySize > targetSize {
				return nil, errors.New("delta copy out of bounds")
			}
			copy(out[written:written+copySize], base[copyOffset:copyOffset+copySize])
			written += copySize
		} else if op != 0 {
			lit := int(op & 0x7f)
			if offset+lit > len(delta) || written+lit > targetSize {
				return nil, errors.New("delta literal out of bounds")
			}
			copy(out[written:written+lit], delta[offset:offset+lit])
			offset += lit
			written += lit
		} else {
			return nil, errors.New("delta opcode 0 invalid")
		}
	}
	if written != targetSize {
		return nil, errors.New("delta produced unexpected size")
	}
	return out, nil
}

type RawPackEntry struct {
	Offset     int
	TypeCode   int
	Size       int
	Data       []byte
	BaseOffset int
	BaseOID    string
}

func ReadPack(pack []byte, resolveBase func(oid string) *GitObject) (map[string]GitObject, error) {
	if len(pack) < 32 {
		return nil, errors.New("pack too short")
	}
	if string(pack[0:4]) != "PACK" {
		return nil, errors.New("invalid pack header")
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	declaredCount := int(binary.BigEndian.Uint32(pack[8:12]))

	trailerStart := len(pack) - 20
	var rawEntries []RawPackEntry
	byOffset := make(map[int]*RawPackEntry)

	offset := 12
	for i := 0; i < declaredCount; i++ {
		entryOffset := offset
		size, next, first, err := readVarintSize(pack, offset)
		if err != nil {
			return nil, err
		}
		offset = next
		typeCode := int((first >> 4) & 0x07)
		entry := RawPackEntry{Offset: entryOffset, TypeCode: typeCode, Size: size}

		if typeCode == 6 {
			baseOfs, nextOfs, err := readOfsDeltaBase(pack, offset)
			if err != nil {
				return nil, err
			}
			offset = nextOfs
			entry.BaseOffset = entryOffset - baseOfs
		} else if typeCode == 7 {
			if offset+20 > trailerStart {
				return nil, errors.New("ref-delta base id truncated")
			}
			entry.BaseOID = hex.EncodeToString(pack[offset : offset+20])
			offset += 20
		}

		// Inflate zlib payload
		reader := &byteSliceReader{data: pack[offset:trailerStart]}
		zr, err := zlib.NewReader(reader)
		if err != nil {
			return nil, err
		}
		decomp := make([]byte, size)
		_, err = io.ReadFull(zr, decomp)
		if err != nil {
			return nil, err
		}
		zr.Close()
		offset += reader.pos
		entry.Data = decomp
		rawEntries = append(rawEntries, entry)
	}

	for i := range rawEntries {
		byOffset[rawEntries[i].Offset] = &rawEntries[i]
	}

	objects := make(map[string]GitObject)
	var resolve func(e *RawPackEntry) (GitObjectType, []byte, string, error)
	resolve = func(e *RawPackEntry) (GitObjectType, []byte, string, error) {
		var objType GitObjectType
		var data []byte
		if e.TypeCode == 6 {
			baseEntry := byOffset[e.BaseOffset]
			if baseEntry == nil {
				return "", nil, "", errors.New("ofs-delta base not found")
			}
			bType, bData, _, err := resolve(baseEntry)
			if err != nil {
				return "", nil, "", err
			}
			applied, err := ApplyDelta(bData, e.Data)
			if err != nil {
				return "", nil, "", err
			}
			objType = bType
			data = applied
		} else if e.TypeCode == 7 {
			var bType GitObjectType
			var bData []byte
			baseEntry := byOffset[0] // search in raw
			for i := range rawEntries {
				if HashObject(TypeCommit, rawEntries[i].Data) == e.BaseOID ||
					HashObject(TypeTree, rawEntries[i].Data) == e.BaseOID ||
					HashObject(TypeBlob, rawEntries[i].Data) == e.BaseOID {
					baseEntry = &rawEntries[i]
					break
				}
			}
			if baseEntry != nil {
				var err error
				bType, bData, _, err = resolve(baseEntry)
				if err != nil {
					return "", nil, "", err
				}
			} else if resolveBase != nil {
				ext := resolveBase(e.BaseOID)
				if ext == nil {
					return "", nil, "", fmt.Errorf("ref-delta base %s not found", e.BaseOID)
				}
				bType = ext.Type
				bData = ext.Data
			} else {
				return "", nil, "", fmt.Errorf("ref-delta base %s not found", e.BaseOID)
			}
			applied, err := ApplyDelta(bData, e.Data)
			if err != nil {
				return "", nil, "", err
			}
			objType = bType
			data = applied
		} else {
			switch e.TypeCode {
			case 1:
				objType = TypeCommit
			case 2:
				objType = TypeTree
			case 3:
				objType = TypeBlob
			case 4:
				objType = TypeTag
			default:
				return "", nil, "", fmt.Errorf("unknown type code %d", e.TypeCode)
			}
			data = e.Data
		}
		oid := HashObject(objType, data)
		return objType, data, oid, nil
	}

	for i := range rawEntries {
		t, d, oid, err := resolve(&rawEntries[i])
		if err != nil {
			return nil, err
		}
		objects[oid] = GitObject{Type: t, Data: d, OID: oid}
	}

	return objects, nil
}
