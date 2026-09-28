package git

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// pkt-line framing as used by the git smart protocols: a four-digit hex
// length (including the prefix) followed by the payload, with three special
// zero-payload packets.
const (
	maxPktLength     = 65520
	maxPktData       = maxPktLength - 4
	maxSideBandData  = maxPktData - 1
	sideBandData     = 1
	sideBandProgress = 2
	sideBandError    = 3
)

type pktKind uint8

const (
	pktData pktKind = iota
	pktFlush
	pktDelim
	pktResponseEnd
)

var errPktTooLong = errors.New("pkt-line exceeds the 65520 byte limit")

// readPkt reads one packet. It returns io.EOF only at a clean end of stream
// between packets; a truncated packet is an io.ErrUnexpectedEOF.
func readPkt(r io.Reader) (pktKind, []byte, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return pktData, nil, io.ErrUnexpectedEOF
		}
		return pktData, nil, err
	}
	var decoded [2]byte
	if _, err := hex.Decode(decoded[:], prefix[:]); err != nil {
		return pktData, nil, fmt.Errorf("invalid pkt-line length %q", prefix[:])
	}
	length := int(decoded[0])<<8 | int(decoded[1])
	switch length {
	case 0:
		return pktFlush, nil, nil
	case 1:
		return pktDelim, nil, nil
	case 2:
		return pktResponseEnd, nil, nil
	}
	if length < 4 {
		return pktData, nil, fmt.Errorf("invalid pkt-line length %d", length)
	}
	if length > maxPktLength {
		return pktData, nil, errPktTooLong
	}
	data := make([]byte, length-4)
	if _, err := io.ReadFull(r, data); err != nil {
		return pktData, nil, io.ErrUnexpectedEOF
	}
	return pktData, data, nil
}

// readPktLine reads one data packet and strips a single trailing LF. Special
// packets are reported through kind with an empty line.
func readPktLine(r io.Reader) (pktKind, string, error) {
	kind, data, err := readPkt(r)
	if err != nil || kind != pktData {
		return kind, "", err
	}
	return pktData, string(bytes.TrimSuffix(data, []byte{'\n'})), nil
}

func writePkt(w io.Writer, data []byte) error {
	if len(data) > maxPktData {
		return errPktTooLong
	}
	var prefix [4]byte
	hex.Encode(prefix[:], []byte{byte((len(data) + 4) >> 8), byte(len(data) + 4)})
	if _, err := w.Write(prefix[:]); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

func writePktLine(w io.Writer, line string) error {
	return writePkt(w, []byte(line+"\n"))
}

func writeFlush(w io.Writer) error {
	_, err := io.WriteString(w, "0000")
	return err
}

func writeDelim(w io.Writer) error {
	_, err := io.WriteString(w, "0001")
	return err
}

// writeSideBand frames data on one side-band channel, splitting it into
// packets that fit the pkt-line limit.
func writeSideBand(w io.Writer, band byte, data []byte) error {
	for len(data) > 0 {
		chunk := data
		if len(chunk) > maxSideBandData {
			chunk = data[:maxSideBandData]
		}
		if err := writePkt(w, append([]byte{band}, chunk...)); err != nil {
			return err
		}
		data = data[len(chunk):]
	}
	return nil
}

// sideBandWriter frames every Write on the data channel.
type sideBandWriter struct {
	w io.Writer
}

func (s sideBandWriter) Write(p []byte) (int, error) {
	if err := writeSideBand(s.w, sideBandData, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// pktBuffer accumulates a pkt-line request body.
type pktBuffer struct {
	bytes.Buffer
}

func (b *pktBuffer) line(line string) *pktBuffer {
	_ = writePktLine(&b.Buffer, line)
	return b
}

func (b *pktBuffer) delim() *pktBuffer {
	_ = writeDelim(&b.Buffer)
	return b
}

func (b *pktBuffer) flush() *pktBuffer {
	_ = writeFlush(&b.Buffer)
	return b
}
