package testutil

import (
	"bytes"
	"encoding/binary"
)

// OggVorbisFile frames raw Vorbis packets into an Ogg-Vorbis byte stream for
// ffmpeg (or another decoder) to read. It is TEST-ONLY scaffolding so the Vorbis
// encoder can be exercised through a real container before the production
// Ogg-Vorbis muxer exists (that muxer, with exact gapless granulepos, is a later
// phase). The granule positions here are approximate: they are enough for a
// decoder to produce PCM, not the exact trims a player would honor.
//
// id/comment/setup are the three Vorbis headers; packets are the audio packets;
// granules[k] is the cumulative decoded sample count after packet k (the encoder
// stamps each packet's decoded length in Dur, so this is a running sum that
// carries variable block sizes correctly); samples is the true length, stamped on
// the final (EOS) page.
func OggVorbisFile(id, comment, setup []byte, packets [][]byte, granules []int64, samples int64) []byte {
	const serial = 0x57415846 // "WAXF"
	var out bytes.Buffer
	oggPage(&out, serial, 0, 0x02, 0, id) // BOS: identification packet alone
	oggPage(&out, serial, 1, 0x00, 0, comment)
	oggPage(&out, serial, 2, 0x00, 0, setup)
	seq := uint32(3)
	for k, pkt := range packets {
		gran := int64(0)
		if k < len(granules) {
			gran = granules[k]
		}
		if gran > samples {
			gran = samples
		}
		ht := byte(0)
		if k == len(packets)-1 {
			ht = 0x04 // EOS
			gran = samples
		}
		oggPage(&out, serial, seq, ht, gran, pkt)
		seq++
	}
	return out.Bytes()
}

// oggPage frames one packet as a single Ogg page (one packet per page keeps the
// packer trivial; decoders accept it).
func oggPage(out *bytes.Buffer, serial, seq uint32, headerType byte, granule int64, packet []byte) {
	var segs []byte
	n := len(packet)
	for n >= 255 {
		segs = append(segs, 255)
		n -= 255
	}
	segs = append(segs, byte(n))

	page := make([]byte, 27+len(segs))
	copy(page, "OggS")
	page[5] = headerType
	binary.LittleEndian.PutUint64(page[6:], uint64(granule))
	binary.LittleEndian.PutUint32(page[14:], serial)
	binary.LittleEndian.PutUint32(page[18:], seq)
	page[26] = byte(len(segs))
	copy(page[27:], segs)
	page = append(page, packet...)
	binary.LittleEndian.PutUint32(page[22:], oggCRC(page))
	out.Write(page)
}

// oggCRC is the RFC 3533 page checksum (polynomial 0x04C11DB7, no reflection).
var oggCRCTable = func() (t [256]uint32) {
	for i := range t {
		crc := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
		t[i] = crc
	}
	return
}()

func oggCRC(data []byte) uint32 {
	var crc uint32
	for _, b := range data {
		crc = crc<<8 ^ oggCRCTable[byte(crc>>24)^b]
	}
	return crc
}
