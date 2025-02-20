package rawmessage

import (
	"time"

	"github.com/luiscastelli/cad-rtsp-simple-server/internal/rtmp/chunk"
)

// Message is a raw message.
type Message struct {
	ChunkStreamID   byte
	Timestamp       time.Duration
	Type            chunk.MessageType
	MessageStreamID uint32
	Body            []byte
}
