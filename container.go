package qp

import (
	"encoding"
	"encoding/binary"
	"errors"
	"io"
	"sync"
)

const (
	DefaultMinBuf = 2048
)

// ErrPayloadTooShort indicates that the message was not complete.
var ErrPayloadTooShort = errors.New("payload too short")
var ErrMessageTooBig = errors.New("message size larger than buffer")

// Default is the protocol used by the raw Encode and Decode functions.
var Default = NineP2000

// Protocol defines a protocol message encoder/decoder
type Protocol interface {
	Decode(r io.Reader) (Message, error)
	Encode(w io.Writer, m Message) error
	MessageType(Message) (MessageType, error)
	Message(MessageType) (Message, error)
}

// MessageType is the type of the contained message.
type MessageType byte

// Message is an interface describing an item that can encode itself to a
// writer, decode itself from a reader, and inform how large the encoded form
// would be at the current time. It is also capable of getting/setting the
// message tag, which is merely a convenience feature to save a type assert
// for access to the tag.
type Message interface {
	encoding.BinaryUnmarshaler
	encoding.BinaryMarshaler
	GetTag() Tag
}

// Write write all the provided data unless and io error occurs.
func write(w io.Writer, b []byte) error {
	var (
		written int
		err     error
		l       = len(b)
	)
	for written < l {
		written, err = w.Write(b[written:])
		if err != nil {
			return err
		}
	}

	return nil
}

// DecodeHdr reads 5 bytes and returns the decoded size and message type. It
// may return an error if reading from the Reader fails.
func DecodeHdr(r io.Reader) (uint32, MessageType, error) {
	var (
		n    int
		size uint32
		mt   MessageType
		err  error
	)

	b := make([]byte, 5)
	n, err = io.ReadFull(r, b)
	if n < 5 {
		return 0, 0, err
	}
	size = binary.LittleEndian.Uint32(b[0:4])
	mt = MessageType(b[4])
	return size, mt, err
}

// Codec encodes/decodes messages using the provided message type <-> message
// conversion.
type Codec struct {
	M2MT func(Message) (MessageType, error)
	MT2M func(MessageType) (Message, error)
}

// Decode decodes an entire message, including header, and returns the message.
// It may return an error if reading from the Reader fails, or if a message
// tries to consume more data than the size of the header indicated, making the
// message invalid.
func (c *Codec) Decode(r io.Reader) (Message, error) {
	var (
		size uint32
		mt   MessageType
		err  error
	)
	if size, mt, err = DecodeHdr(r); err != nil {
		return nil, err
	}

	size -= HeaderSize

	b := make([]byte, size)
	n, err := io.ReadFull(r, b)
	if err != nil {
		return nil, err
	}
	if n != int(size) {
		return nil, errors.New("short read")
	}

	m, err := c.MT2M(mt)
	if err != nil {
		return nil, err
	}

	if err = m.UnmarshalBinary(b); err != nil {
		return nil, err
	}
	return m, nil
}

// Encode write a header and message to the provided writer. It returns an
// error if writing failed.
func (c *Codec) Encode(w io.Writer, m Message) error {
	var err error
	var mt MessageType
	if mt, err = c.M2MT(m); err != nil {
		return err
	}

	var b []byte
	if b, err = m.MarshalBinary(); err != nil {
		return err
	}

	h := make([]byte, 5)
	binary.LittleEndian.PutUint32(h[0:4], uint32(len(b)+HeaderSize))
	h[4] = byte(mt)

	if err = write(w, h); err != nil {
		return err
	}

	if err = write(w, b); err != nil {
		return err
	}

	return nil
}

func (c *Codec) MessageType(m Message) (MessageType, error) {
	return c.M2MT(m)
}

func (c *Codec) Message(mt MessageType) (Message, error) {
	return c.MT2M(mt)
}

// Decode is a convenience function for calling decode on the default
// protocol.
func Decode(r io.Reader) (Message, error) {
	return Default.Decode(r)
}

// Encode is a convenience function for calling encode on the default
// protocol.
func Encode(w io.Writer, d Message) error {
	return Default.Encode(w, d)
}

// Encoder handles writes encoded messages to an io.Writer.
type Encoder struct {
	Protocol Protocol
	Writer   io.Writer
	MaxSize  int
	Sloppy   bool

	writeLock sync.Mutex
}

// SetProtocol sets the protocol codec of the Encoder.
func (e *Encoder) SetProtocol(p Protocol) {
	e.writeLock.Lock()
	defer e.writeLock.Unlock()

	e.Protocol = p
}

// SetWriter replaces the io.Writer of the Encoder.
func (e *Encoder) SetWriter(w io.Writer) {
	e.writeLock.Lock()
	defer e.writeLock.Unlock()

	e.Writer = w
}

// WriteMessage encodes a message and writes it to the Encoders associated
// io.Writer.
func (e *Encoder) WriteMessage(m Message) error {
	var (
		mt  MessageType
		buf []byte
		err error
	)

	if mt, err = e.Protocol.MessageType(m); err != nil {
		return err
	}

	if buf, err = m.MarshalBinary(); err != nil {
		return err
	}

	if !e.Sloppy && (len(buf)+HeaderSize) > e.MaxSize {
		return ErrMessageTooBig
	}

	header := make([]byte, HeaderSize)
	binary.LittleEndian.PutUint32(header[0:4], uint32(len(buf)+HeaderSize))
	header[4] = byte(mt)

	if err = write(e.Writer, header); err != nil {
		return err
	}

	if err = write(e.Writer, buf); err != nil {
		return err
	}

	return nil
}

// Decoder reads messages from an io.Reader, calling a callback for each of them.
// message. Unlike Codec.Decode,
type Decoder struct {
	Protocol Protocol
	Callback func(m Message) error
	Reader   io.Reader
	Stopped  bool
	MaxSize  int
	MinBuf   int
	Sloppy   bool
}

// SetProtocol sets the protocol codec of the Decoder. Replacing the protocol
// codec is well defined, assuming that the caller knows that no further
// messages will be decoded before the replace have occured. A way to ensure
// this is to call SetProtocol from a blocking callback.
func (d *Decoder) SetProtocol(p Protocol) {
	d.Protocol = p
}

// SetReader replaces the io.Reader of the Decoder. Replacing the reader is
// well-defined if the caller is certain that the server have not sent and
// will not send any further messages before the replace have occured.
// Otherwise, a partial message may be left in the buffer, breaking the
// Decoder.
func (d *Decoder) SetReader(r io.Reader) {
	d.Reader = r
}

// If the callback returns an error, the reader returns an error, the message
// type is invalid or the message fails to decode, the loop exits with an
// error.
func (d *Decoder) Run() error {
	d.Stopped = false

	if d.MinBuf == 0 {
		d.MinBuf = DefaultMinBuf
	}

	var (
		// total is the count of bytes in the buffer. It is used to keep track
		// of buffer usage (read offset and cleanup), and is not used by the
		// actual decoding loop.
		total uint32

		// needed is the amount of bytes missing. It should only be
		// incremented or decremented, never assigned. This ensures that more
		// than one message can be read into the buffer before the decoder is
		// ready to read them. If needed is zero, it means that all the
		// requested data have been read. If it is negative, then more data
		// then the request has been read. The decoding loop continues until
		// needed is > 0.
		needed int = HeaderSize

		// size is the decoded message body size, not including message
		// header. It is stored in order to increment the ptr correctly after
		// reading a message.
		size uint32

		// ptr is the start index at the buffer at the current time. It is
		// incremented as we read through the buffer, and reset when we clean
		// the buffer.
		ptr uint32

		// m is the current decoded message. It is nil when we have not yet
		// decoded a header, and set to an zero-initialized message struct
		// when we have received the header. Comparing it to nil is used to
		// check what state the decoder is in (header decoding vs. body
		// decoding).
		m Message

		// buf is the reading buffer.
		buf = make([]byte, d.MaxSize)
	)

	for !d.Stopped {
		n, err := d.Reader.Read(buf[total:])
		if err != nil {
			return err
		}

		total += uint32(n)
		needed -= n

		// Handle the data we got
		for needed <= 0 {
			if m == nil { // Read a header if no message struct is set.
				size = binary.LittleEndian.Uint32(buf[ptr:ptr+4]) - HeaderSize
				mt := MessageType(buf[ptr+4])

				// Update message body size, missing bytes and the current ptr.
				needed += int(size)
				ptr += HeaderSize

				// We try to fetch the message struct immediately - better to fail
				// early rather than late.
				if m, err = d.Protocol.Message(mt); err != nil {
					return err
				}

			} else { // Otherwise, read a body for the message.
				if err = m.UnmarshalBinary(buf[ptr : ptr+size]); err != nil {
					return err
				}
				if err = d.Callback(m); err != nil {
					return err
				}
				m = nil

				needed += HeaderSize
				ptr += size
				size = 0
			}
		}

		// Buffer checks and reset.
		l := len(buf)
		remaining := l - int(total)
		if -needed > l {
			// The message is longer than the buffer size, so we need to do
			// *something*.

			if !d.Sloppy {
				// This is considered a fatal error, as the other party must
				// obey the negotiated maxsize as per 9P spec.
				return ErrMessageTooBig
			}

			// Okay, we're being sloppy, so instead of failing, we scale the
			// buffer.
			for -needed > l {
				l *= 2
			}

			// Allocate new buffer and copy the content.
			newbuf := make([]byte, l)
			copy(newbuf, buf[ptr:total])
			buf = newbuf
			total -= ptr
			ptr = 0
		} else if needed > remaining || remaining < d.MinBuf {
			// The remaining part of the buffer is smaller than what we need, or
			// smaller than the minimum hint - time for a cleaning.
			copy(buf, buf[ptr:total])
			total -= ptr
			ptr = 0
		}
	}

	return nil
}

// Stop stops the decoder.
func (d *Decoder) Stop() {
	d.Stopped = true
}
