package protocol

import (
	"encoding/binary"

	"github.com/gravitational/trace"
)

type SQLBatchPacket struct {
	Query string
}

func ParseSQLBatchPacket(pkt *Packet) (*SQLBatchPacket, error) {
	headersLength := binary.LittleEndian.Uint32(pkt.Data[0:4])

	// Skip headers and read query text that goes after.
	query, err := ucs22str(pkt.Data[headersLength:])
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return &SQLBatchPacket{
		Query: query,
	}, nil
}
