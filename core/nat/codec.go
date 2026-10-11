package nat

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

// 候选地址线格式常量(AGENTS.md 8.6.3 的候选交换)。
//
// 布局:
//
//	count(1) + [ type(1) + ip(4|16) + port(2) ] * count
//
// 单个候选 7 字节(IPv4)或 19 字节(IPv6)。为控制握手包体积,
// EncodeCandidates 最多编码 MaxCandidates 条。
const (
	// MaxCandidates 是单次交换允许携带的最大候选数。
	MaxCandidates = 16
	// candidateIPv4Size 是单个 IPv4 候选的线长:type(1)+ip(4)+port(2)。
	candidateIPv4Size = 1 + 4 + 2
	// candidateIPv6Size 是单个 IPv6 候选的线长:type(1)+ip(16)+port(2)。
	candidateIPv6Size = 1 + 16 + 2
	// familyIPv4 / familyIPv6 是地址族标记。
	familyIPv4 byte = 4
	familyIPv6 byte = 6
)

// 候选编解码错误。
var (
	// ErrCandidateDecode 表示候选地址字节流无法解析。
	ErrCandidateDecode = errors.New("nat: candidate decode error")
)

// EncodeCandidates 把候选地址编码为紧凑字节流。
//
// 超过 MaxCandidates 的部分被忽略(按现有顺序,调用方应预先按优先级排序)。
// 空列表编码为单字节 0。
func EncodeCandidates(cands []Candidate) []byte {
	if len(cands) > MaxCandidates {
		cands = cands[:MaxCandidates]
	}

	out := make([]byte, 0, 1+len(cands)*candidateIPv6Size)
	out = append(out, byte(len(cands)))

	for _, c := range cands {
		addr := c.Addr
		if !addr.IsValid() {
			continue
		}
		ip := addr.Addr()
		typ := typeByte(c.Type)

		if ip.Is4() {
			b := ip.As4()
			out = append(out, typ, familyIPv4)
			out = append(out, b[:]...)
		} else {
			b := ip.As16()
			out = append(out, typ, familyIPv6)
			out = append(out, b[:]...)
		}
		var port [2]byte
		binary.BigEndian.PutUint16(port[:], addr.Port())
		out = append(out, port[:]...)
	}

	// 实际写入数量可能少于声明(无效地址被跳过),回填真实计数。
	out[0] = byte(countEntries(out))
	return out
}

// countEntries 统计已编码的候选数量。
func countEntries(buf []byte) int {
	n := 0
	i := 1
	for i < len(buf) {
		typ := buf[i]
		_ = typ
		i++
		if i >= len(buf) {
			break
		}
		fam := buf[i]
		i++
		switch fam {
		case familyIPv4:
			i += 4
		case familyIPv6:
			i += 16
		default:
			return n
		}
		i += 2
		n++
	}
	return n
}

// DecodeCandidates 解析 EncodeCandidates 产出的字节流。
func DecodeCandidates(buf []byte) ([]Candidate, error) {
	if len(buf) == 0 {
		return nil, nil
	}
	if buf[0] == 0 {
		return nil, nil
	}

	count := int(buf[0])
	i := 1
	out := make([]Candidate, 0, count)

	for n := 0; n < count; n++ {
		if i >= len(buf) {
			return out, fmt.Errorf("%w: truncated at candidate %d", ErrCandidateDecode, n)
		}
		typ := typeFromByte(buf[i])
		i++
		if i >= len(buf) {
			return out, fmt.Errorf("%w: truncated family at %d", ErrCandidateDecode, n)
		}
		fam := buf[i]
		i++

		var addr netip.Addr
		switch fam {
		case familyIPv4:
			if i+4 > len(buf) {
				return out, fmt.Errorf("%w: truncated ipv4 at %d", ErrCandidateDecode, n)
			}
			addr = netip.AddrFrom4([4]byte(buf[i : i+4]))
			i += 4
		case familyIPv6:
			if i+16 > len(buf) {
				return out, fmt.Errorf("%w: truncated ipv6 at %d", ErrCandidateDecode, n)
			}
			var b [16]byte
			copy(b[:], buf[i:i+16])
			addr = netip.AddrFrom16(b)
			i += 16
		default:
			return out, fmt.Errorf("%w: bad family %d at candidate %d", ErrCandidateDecode, fam, n)
		}

		if i+2 > len(buf) {
			return out, fmt.Errorf("%w: truncated port at %d", ErrCandidateDecode, n)
		}
		port := binary.BigEndian.Uint16(buf[i : i+2])
		i += 2

		out = append(out, Candidate{
			Type:     typ,
			Addr:     netip.AddrPortFrom(addr, port),
			Priority: priority(typ, addr),
		})
	}
	return out, nil
}

// typeByte / typeFromByte 在 CandidateType 与线格式字节间转换。
func typeByte(t CandidateType) byte {
	switch t {
	case CandidateHost:
		return 1
	case CandidateSrflx:
		return 2
	case CandidateRelay:
		return 3
	default:
		return 0
	}
}

func typeFromByte(b byte) CandidateType {
	switch b {
	case 1:
		return CandidateHost
	case 2:
		return CandidateSrflx
	case 3:
		return CandidateRelay
	default:
		return CandidateType("")
	}
}
