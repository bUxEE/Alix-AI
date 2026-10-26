package mistral

import (
	"encoding/binary"

	"github.com/buxee/alix-ai/internal/providers/voice"
)

const wavHeaderBytes = 44

func wavHeader(f voice.AudioFormat, dataLen int) []byte {
	h := make([]byte, wavHeaderBytes)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+dataLen))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], uint16(f.Channels))
	binary.LittleEndian.PutUint32(h[24:], uint32(f.SampleRate))
	binary.LittleEndian.PutUint32(h[28:], uint32(f.BytesPerSecond()))
	binary.LittleEndian.PutUint16(h[32:], uint16(f.FrameBytes()))
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(dataLen))
	return h
}
