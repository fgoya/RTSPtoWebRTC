package nal

import (
	"bytes"
)

// Same as binary.BigEndian.Uint32()
func next32(b []byte) uint32 {
	_ = b[3] // bounds check hint to compiler; see golang.org/issue/14808
	return uint32(b[3]) | uint32(b[2])<<8 | uint32(b[1])<<16 | uint32(b[0])<<24
}

// func next24(b []byte) uint32 {
// 	_ = b[2] // bounds check hint to compiler; see golang.org/issue/14808
// 	return uint32(b[2]) | uint32(b[1])<<8 | uint32(b[0])<<16
// }

type Unit []byte

// forbidden_zero_bit.  A value of 0 indicates that the NAL unit
// type octet and payload should not contain bit errors or other
// syntax violations.  A value of 1 indicates that the NAL unit
// type octet and payload may contain bit errors or other syntax
// violations.
func (u Unit) F() bool {
	return u[0]&0x80 != 0x00
}

// nal_ref_idc.  The semantics of value 00 and a non-zero value
// remain unchanged from the H.264 specification.  In other words,
// a value of 00 indicates that the content of the NAL unit is not
// used to reconstruct reference pictures for inter picture
// prediction.  Such NAL units can be discarded without risking
// the integrity of the reference pictures.  Values greater than
// 00 indicate that the decoding of the NAL unit is required to
// maintain the integrity of the reference pictures.
func (u Unit) NRI() byte {
	return u[0] >> 5 & 0x03
}

// nal_unit_type
func (u Unit) Type() byte {
	return u[0] & 0x1F
}

// Raw Byte Sequence Payload
func (u Unit) Payload() []byte {
	return u[1:]
}

func (u Unit) IsZero() bool {
	return len(u) == 0
}

// Byte stream splitter
func AnnexBSplit(b []byte) (units []Unit, leading bool) {

	// Not sure if it is faster this way
	r := bytes.Split(b, []byte{0x00, 0x00, 0x01})
	if len(r) == 0 { // I don't think this happens
		return nil, false
	}

	res := make([]Unit, 0, len(r))
	for i, last := 0, len(r)-1; i <= last; i++ {
		v := r[i]
		if i != last && len(v) > 0 && v[len(v)-1] == 0x00 {
			res = append(res, Unit(v[:len(v)-1]))
		} else {
			res = append(res, Unit(v))
		}
	}

	if len(res[0]) == 0 {
		return res[1:], true
	}
	return res, false

	// var res []Unit
	// i := 0
	// for i < len(b) {
	// 	if n := len(b) - i; n >= 4 {
	// 		if next32(b[i:]) == 0x0000001 {
	// 			if i > 0 {
	// 				res = append(res, Unit(b[:i]))
	// 			}
	// 			b = b[i+4:]
	// 			i = 0
	// 			continue
	// 		} else if next24(b[i:]) == 0x00001 {
	// 			if i > 0 {
	// 				res = append(res, Unit(b[:i]))
	// 			}
	// 			b = b[i+3:]
	// 			i = 0
	// 			continue
	// 		}
	// 	} else if n == 3 {
	// 		if next24(b[i:]) == 0x00001 {
	// 			if i > 0 {
	// 				res = append(res, Unit(b[:i]))
	// 			}
	// 			b = b[i+3:]
	// 			i = 0
	// 			continue
	// 		}
	// 	}
	// 	i++
	// }
	// if i > 0 {
	// 	res = append(res, Unit(b[:i]))
	// }

	// return res
}

func AVCCSplit(b []byte) []Unit {

	var units []Unit
	for len(b) >= 4 {
		n := next32(b)
		b = b[4:]
		if n == 0 || uint32(len(b)) < n {
			break
		}
		units = append(units, b[:n])
		b = b[n:]
		if len(b) == 0 {
			return units
		}
	}
	return nil
}

func CompatibleSplit(b []byte, presume bool) []Unit {

	units := AVCCSplit(b)
	if len(units) > 0 {
		return units
	}

	units, leading := AnnexBSplit(b)
	if leading || presume {
		return units
	}

	return []Unit{b}
}
