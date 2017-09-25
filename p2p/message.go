package p2p

import (
	"bufio"
	"context"
	"fmt"

	net "github.com/libp2p/go-libp2p-net"
	pstore "github.com/libp2p/go-libp2p-peerstore"
	// ma "github.com/multiformats/go-multiaddr"
	multicodec "github.com/multiformats/go-multicodec"
	json "github.com/multiformats/go-multicodec/json"
)

type MsgType int

const (
	MtUnknown MsgType = iota
	MtPeerInfo
	MtDatasets
	MtNamespaces
	MtSearch
)

type MsgPhase int

const (
	MpRequest MsgPhase = iota
	MpResponse
	MpError
)

// Message is a serializable/encodable object that we will send
// on a Stream.
type Message struct {
	Type    MsgType
	Phase   MsgPhase
	Payload interface{}
	HangUp  bool
}

// streamWrap wraps a libp2p stream. We encode/decode whenever we
// write/read from a stream, so we can just carry the encoders
// and bufios with us
type WrappedStream struct {
	stream net.Stream
	enc    multicodec.Encoder
	dec    multicodec.Decoder
	w      *bufio.Writer
	r      *bufio.Reader
}

// wrapStream takes a stream and complements it with r/w bufios and
// decoder/encoder. In order to write raw data to the stream we can use
// wrap.w.Write(). To encode something into it we can wrap.enc.Encode().
// Finally, we should wrap.w.Flush() to actually send the data. Handling
// incoming data works similarly with wrap.r.Read() for raw-reading and
// wrap.dec.Decode() to decode.
func WrapStream(s net.Stream) *WrappedStream {
	reader := bufio.NewReader(s)
	writer := bufio.NewWriter(s)
	// This is where we pick our specific multicodec. In order to change the
	// codec, we only need to change this place.
	// See https://godoc.org/github.com/multiformats/go-multicodec/json
	dec := json.Multicodec(false).Decoder(reader)
	enc := json.Multicodec(false).Encoder(writer)
	return &WrappedStream{
		stream: s,
		r:      reader,
		w:      writer,
		enc:    enc,
		dec:    dec,
	}
}

// StreamHandler handles connections to this node
func (qn *QriNode) MessageStreamHandler(s net.Stream) {
	defer s.Close()
	qn.handleStream(WrapStream(s))
}

// SendMessage to a given multiaddr
func (qn *QriNode) SendMessage(pi pstore.PeerInfo, msg *Message) (res *Message, err error) {
	s, err := qn.Host.NewStream(context.Background(), pi.ID, QriProtocolId)
	if err != nil {
		return
	}
	defer s.Close()

	wrappedStream := WrapStream(s)

	msg.Phase = MpRequest
	err = sendMessage(msg, wrappedStream)
	if err != nil {
		return
	}

	return receiveMessage(wrappedStream)
}

// BroadcastMessage sends a message to all connected peers
func (qn *QriNode) BroadcastMessage(msg *Message) (res *Message, err error) {
	peers := qn.Peerstore.Peers()
	for _, p := range peers {
		qn.SendMessage(qn.Peerstore.PeerInfo(p), msg)
	}
	return nil, fmt.Errorf("not finished: broadcast message")
}

// receiveMessage reads and decodes a message from the stream
func receiveMessage(ws *WrappedStream) (*Message, error) {
	var msg Message
	err := ws.dec.Decode(&msg)
	if err != nil {
		return nil, err
	}
	return &msg, nil
}

// // sendMessage encodes and writes a message to the stream
func sendMessage(msg *Message, ws *WrappedStream) error {
	err := ws.enc.Encode(msg)
	// Because output is buffered with bufio, we need to flush!
	ws.w.Flush()
	return err
}

// handleStream is a for loop which receives and then sends a message.
// When Message.HangUp is true, it exits. This will close the stream
// on one of the sides. The other side's receiveMessage() will error
// with EOF, thus also breaking out from the loop.
func (n *QriNode) handleStream(ws *WrappedStream) {
	for {
		// Read
		r, err := receiveMessage(ws)
		if err != nil {
			break
		}
		fmt.Printf("received message: %v\n", r)

		var res *Message

		if r.Phase == MpRequest {
			switch r.Type {
			case MtPeerInfo:
				res = n.handlePeerInfoRequest(r)
			case MtDatasets:
				res = n.handleDatasetsRequest(r)
			case MtSearch:
			}
		}

		if res != nil {
			if err := sendMessage(res, ws); err != nil {
				fmt.Println("send message error", err)
			}
		}

		if r.HangUp {
			break
		}
	}
}