package events

import (
	"encoding/binary"
	"fmt"
	"math"
)

// DecodeCBOR decodes one CBOR item and returns the remainder. Nuncio only needs
// enough of CBOR to read ATProto firehose frame headers and bodies, so this is a
// deliberately small, strict decoder rather than a general library.
func DecodeCBOR(data []byte) (any, []byte, error) {
	if len(data) == 0 {
		return nil, nil, fmt.Errorf("cbor: empty input")
	}
	major := data[0] >> 5
	info := data[0] & 0x1f
	rest := data[1:]
	value, rest, err := readLength(info, rest)
	if err != nil {
		return nil, nil, err
	}
	switch major {
	case 0:
		return value, rest, nil
	case 1:
		return -1 - int64(value), rest, nil
	case 2:
		if info == 31 {
			var out []byte
			for len(rest) > 0 && rest[0] != 0xff {
				item, next, err := DecodeCBOR(rest)
				if err != nil {
					return nil, nil, err
				}
				chunk, ok := item.([]byte)
				if !ok {
					return nil, nil, fmt.Errorf("cbor: indefinite bytes chunk is %T", item)
				}
				out = append(out, chunk...)
				rest = next
			}
			if len(rest) == 0 {
				return nil, nil, fmt.Errorf("cbor: unterminated bytes")
			}
			return out, rest[1:], nil
		}
		if uint64(len(rest)) < value {
			return nil, nil, fmt.Errorf("cbor: truncated bytes")
		}
		return append([]byte{}, rest[:value]...), rest[value:], nil
	case 3:
		if info == 31 {
			out := ""
			for len(rest) > 0 && rest[0] != 0xff {
				item, next, err := DecodeCBOR(rest)
				if err != nil {
					return nil, nil, err
				}
				chunk, ok := item.(string)
				if !ok {
					return nil, nil, fmt.Errorf("cbor: indefinite string chunk is %T", item)
				}
				out += chunk
				rest = next
			}
			if len(rest) == 0 {
				return nil, nil, fmt.Errorf("cbor: unterminated string")
			}
			return out, rest[1:], nil
		}
		if uint64(len(rest)) < value {
			return nil, nil, fmt.Errorf("cbor: truncated string")
		}
		return string(rest[:value]), rest[value:], nil
	case 4:
		var out []any
		if info == 31 {
			for len(rest) > 0 && rest[0] != 0xff {
				item, next, err := DecodeCBOR(rest)
				if err != nil {
					return nil, nil, err
				}
				out = append(out, item)
				rest = next
			}
			if len(rest) == 0 {
				return nil, nil, fmt.Errorf("cbor: unterminated array")
			}
			return out, rest[1:], nil
		}
		for i := uint64(0); i < value; i++ {
			item, next, err := DecodeCBOR(rest)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, item)
			rest = next
		}
		return out, rest, nil
	case 5:
		out := map[string]any{}
		if info == 31 {
			for len(rest) > 0 && rest[0] != 0xff {
				k, next, err := DecodeCBOR(rest)
				if err != nil {
					return nil, nil, err
				}
				key, ok := k.(string)
				if !ok {
					return nil, nil, fmt.Errorf("cbor: map key is %T, want string", k)
				}
				v, next2, err := DecodeCBOR(next)
				if err != nil {
					return nil, nil, err
				}
				out[key] = v
				rest = next2
			}
			if len(rest) == 0 {
				return nil, nil, fmt.Errorf("cbor: unterminated map")
			}
			return out, rest[1:], nil
		}
		for i := uint64(0); i < value; i++ {
			k, next, err := DecodeCBOR(rest)
			if err != nil {
				return nil, nil, err
			}
			key, ok := k.(string)
			if !ok {
				return nil, nil, fmt.Errorf("cbor: map key is %T, want string", k)
			}
			v, next2, err := DecodeCBOR(next)
			if err != nil {
				return nil, nil, err
			}
			out[key] = v
			rest = next2
		}
		return out, rest, nil
	case 6:
		// Tag: decode the tagged item and ignore the tag number.
		return DecodeCBOR(rest)
	case 7:
		switch info {
		case 20:
			return false, rest, nil
		case 21:
			return true, rest, nil
		case 22, 23:
			return nil, rest, nil
		case 25:
			if len(rest) < 2 {
				return nil, nil, fmt.Errorf("cbor: truncated half float")
			}
			return halfToFloat(binary.BigEndian.Uint16(rest)), rest[2:], nil
		case 26:
			if len(rest) < 4 {
				return nil, nil, fmt.Errorf("cbor: truncated float")
			}
			return float64(math.Float32frombits(binary.BigEndian.Uint32(rest))), rest[4:], nil
		case 27:
			if len(rest) < 8 {
				return nil, nil, fmt.Errorf("cbor: truncated double")
			}
			return math.Float64frombits(binary.BigEndian.Uint64(rest)), rest[8:], nil
		}
		return nil, nil, fmt.Errorf("cbor: unsupported simple value %d", info)
	default:
		return nil, nil, fmt.Errorf("cbor: unsupported major type %d", major)
	}
}

func readLength(info byte, rest []byte) (uint64, []byte, error) {
	switch {
	case info < 24:
		return uint64(info), rest, nil
	case info == 24:
		if len(rest) < 1 {
			return 0, nil, fmt.Errorf("cbor: truncated length")
		}
		return uint64(rest[0]), rest[1:], nil
	case info == 25:
		if len(rest) < 2 {
			return 0, nil, fmt.Errorf("cbor: truncated length")
		}
		return uint64(binary.BigEndian.Uint16(rest)), rest[2:], nil
	case info == 26:
		if len(rest) < 4 {
			return 0, nil, fmt.Errorf("cbor: truncated length")
		}
		return uint64(binary.BigEndian.Uint32(rest)), rest[4:], nil
	case info == 27:
		if len(rest) < 8 {
			return 0, nil, fmt.Errorf("cbor: truncated length")
		}
		return binary.BigEndian.Uint64(rest), rest[8:], nil
	case info == 31:
		return 0, rest, nil
	default:
		return 0, nil, fmt.Errorf("cbor: reserved length encoding %d", info)
	}
}

func halfToFloat(h uint16) float64 {
	sign := h >> 15
	exp := (h >> 10) & 0x1f
	frac := h & 0x3ff
	var f float64
	switch exp {
	case 0:
		f = math.Ldexp(float64(frac), -24)
	case 31:
		if frac == 0 {
			f = math.Inf(1)
		} else {
			f = math.NaN()
		}
	default:
		f = math.Ldexp(float64(frac+1024), int(exp)-25)
	}
	if sign == 1 {
		f = -f
	}
	return f
}

// ParseFirehoseFrame reads the two CBOR items in an ATProto subscribeRepos
// frame. The header carries the message type; the body carries seq and repo.
func ParseFirehoseFrame(data []byte) (header map[string]any, body map[string]any, err error) {
	h, rest, err := DecodeCBOR(data)
	if err != nil {
		return nil, nil, err
	}
	hm, ok := h.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("firehose: header is %T", h)
	}
	b, _, err := DecodeCBOR(rest)
	if err != nil {
		return nil, nil, err
	}
	bm, ok := b.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("firehose: body is %T", b)
	}
	return hm, bm, nil
}
