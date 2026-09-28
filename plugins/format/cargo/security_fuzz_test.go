package cargo

import (
	"bytes"
	"testing"
)

func FuzzPublishFrameLength(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{1, 2, 3})
	f.Fuzz(func(t *testing.T, frame []byte) {
		_, _ = readUint32(bytes.NewReader(frame))
	})
}
