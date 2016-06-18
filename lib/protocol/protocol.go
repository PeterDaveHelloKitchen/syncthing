// Copyright (C) 2014 The Protocol Authors.

package protocol

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	lz4 "github.com/bkaradzic/go-lz4"
)

const (
	// BlockSize is the standard ata block size (128 KiB)
	BlockSize = 128 << 10

	// MaxMessageLen is the largest message size allowed on the wire. (512 MiB)
	MaxMessageLen = 64 << 23
)

const (
	stateInitial = iota
	stateReady
)

// Request message flags
const (
	FlagFromTemporary uint32 = 1 << iota
)

// ClusterConfigMessage.Folders flags
const (
	FlagFolderReadOnly            uint32 = 1 << 0
	FlagFolderIgnorePerms                = 1 << 1
	FlagFolderIgnoreDelete               = 1 << 2
	FlagFolderDisabledTempIndexes        = 1 << 3
	FlagFolderAll                        = 1<<4 - 1
)

// ClusterConfigMessage.Folders.Devices flags
const (
	FlagShareTrusted  uint32 = 1 << 0
	FlagShareReadOnly        = 1 << 1
	FlagIntroducer           = 1 << 2
	FlagShareBits            = 0x000000ff
)

var (
	ErrClosed               = errors.New("connection closed")
	ErrTimeout              = errors.New("read timeout")
	ErrSwitchingConnections = errors.New("switching connections")
)

type Model interface {
	// An index was received from the peer device
	Index(deviceID DeviceID, folder string, files []FileInfo)
	// An index update was received from the peer device
	IndexUpdate(deviceID DeviceID, folder string, files []FileInfo)
	// A request was made by the peer device
	Request(deviceID DeviceID, folder string, name string, offset int64, hash []byte, fromTemporary bool, buf []byte) error
	// A cluster configuration message was received
	ClusterConfig(deviceID DeviceID, config ClusterConfigMessage)
	// The peer device closed the connection
	Close(deviceID DeviceID, err error)
	// The peer device sent progress updates for the files it is currently downloading
	DownloadProgress(deviceID DeviceID, folder string, updates []FileDownloadProgressUpdate)
}

type Connection interface {
	Start()
	ID() DeviceID
	Name() string
	Index(folder string, files []FileInfo) error
	IndexUpdate(folder string, files []FileInfo) error
	Request(folder string, name string, offset int64, size int, hash []byte, fromTemporary bool) ([]byte, error)
	ClusterConfig(config ClusterConfigMessage)
	DownloadProgress(folder string, updates []FileDownloadProgressUpdate)
	Statistics() Statistics
	Closed() bool
}

type rawConnection struct {
	id       DeviceID
	name     string
	receiver Model

	cr *countingReader
	cw *countingWriter

	awaiting    map[int32]chan asyncResult
	awaitingMut sync.Mutex

	idxMut sync.Mutex // ensures serialization of Index calls

	nextID    int32
	nextIDMut sync.Mutex

	outbox      chan hdrMsg
	closed      chan struct{}
	once        sync.Once
	pool        sync.Pool
	compression Compression

	readerBuf []byte // used & reused by readMessage
}

type asyncResult struct {
	val []byte
	err error
}

type hdrMsg struct {
	hdr  header
	msg  encodable
	done chan struct{}
}

type encodable interface {
	MarshalTo([]byte) (int, error)
	ProtoSize() int
	Unmarshal([]byte) error
}

const (
	// PingSendInterval is how often we make sure to send a message, by
	// triggering pings if necessary.
	PingSendInterval = 90 * time.Second
	// ReceiveTimeout is the longest we'll wait for a message from the other
	// side before closing the connection.
	ReceiveTimeout = 300 * time.Second
)

func NewConnection(deviceID DeviceID, reader io.Reader, writer io.Writer, receiver Model, name string, compress Compression) Connection {
	cr := &countingReader{Reader: reader}
	cw := &countingWriter{Writer: writer}

	c := rawConnection{
		id:       deviceID,
		name:     name,
		receiver: nativeModel{receiver},
		cr:       cr,
		cw:       cw,
		awaiting: make(map[int32]chan asyncResult),
		outbox:   make(chan hdrMsg),
		closed:   make(chan struct{}),
		pool: sync.Pool{
			New: func() interface{} {
				return make([]byte, BlockSize)
			},
		},
		compression: compress,
	}

	return wireFormatConnection{&c}
}

// Start creates the goroutines for sending and receiving of messages. It must
// be called exactly once after creating a connection.
func (c *rawConnection) Start() {
	go c.readerLoop()
	go c.writerLoop()
	go c.pingSender()
	go c.pingReceiver()
}

func (c *rawConnection) ID() DeviceID {
	return c.id
}

func (c *rawConnection) Name() string {
	return c.name
}

// Index writes the list of file information to the connected peer device
func (c *rawConnection) Index(folder string, idx []FileInfo) error {
	select {
	case <-c.closed:
		return ErrClosed
	default:
	}
	c.idxMut.Lock()
	c.send(messageTypeIndex, &IndexMessage{
		Folder: folder,
		Files:  idx,
	}, nil)
	c.idxMut.Unlock()
	return nil
}

// IndexUpdate writes the list of file information to the connected peer device as an update
func (c *rawConnection) IndexUpdate(folder string, idx []FileInfo) error {
	select {
	case <-c.closed:
		return ErrClosed
	default:
	}
	c.idxMut.Lock()
	c.send(messageTypeIndexUpdate, &IndexMessage{
		Folder: folder,
		Files:  idx,
	}, nil)
	c.idxMut.Unlock()
	return nil
}

// Request returns the bytes for the specified block after fetching them from the connected peer.
func (c *rawConnection) Request(folder string, name string, offset int64, size int, hash []byte, fromTemporary bool) ([]byte, error) {
	c.nextIDMut.Lock()
	id := c.nextID
	c.nextID++
	c.nextIDMut.Unlock()

	c.awaitingMut.Lock()
	if _, ok := c.awaiting[id]; ok {
		panic("id taken")
	}
	rc := make(chan asyncResult, 1)
	c.awaiting[id] = rc
	c.awaitingMut.Unlock()

	ok := c.send(messageTypeRequest, &RequestMessage{
		ID:            id,
		Folder:        folder,
		Name:          name,
		Offset:        offset,
		Size:          int32(size),
		Hash:          hash,
		FromTemporary: fromTemporary,
	}, nil)
	if !ok {
		return nil, ErrClosed
	}

	res, ok := <-rc
	if !ok {
		return nil, ErrClosed
	}
	return res.val, res.err
}

// ClusterConfig send the cluster configuration message to the peer and returns any error
func (c *rawConnection) ClusterConfig(config ClusterConfigMessage) {
	c.send(messageTypeClusterConfig, &config, nil)
}

func (c *rawConnection) Closed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// DownloadProgress sends the progress updates for the files that are currently being downloaded.
func (c *rawConnection) DownloadProgress(folder string, updates []FileDownloadProgressUpdate) {
	c.send(messageTypeDownloadProgress, &DownloadProgressMessage{
		Folder:  folder,
		Updates: updates,
	}, nil)
}

func (c *rawConnection) ping() bool {
	return c.send(messageTypePing, nil, nil)
}

func (c *rawConnection) readerLoop() (err error) {
	defer func() {
		c.close(err)
	}()

	state := stateInitial
	for {
		select {
		case <-c.closed:
			return ErrClosed
		default:
		}

		hdr, msg, err := c.readMessage()
		if err != nil {
			return err
		}

		switch msg := msg.(type) {
		case *ClusterConfigMessage:
			if state != stateInitial {
				return fmt.Errorf("protocol error: cluster config message in state %d", state)
			}
			go c.receiver.ClusterConfig(c.id, *msg)
			state = stateReady

		case *IndexMessage:
			switch hdr.msgType {
			case messageTypeIndex:
				if state != stateReady {
					return fmt.Errorf("protocol error: index message in state %d", state)
				}
				c.handleIndex(*msg)
				state = stateReady

			case messageTypeIndexUpdate:
				if state != stateReady {
					return fmt.Errorf("protocol error: index update message in state %d", state)
				}
				c.handleIndexUpdate(*msg)
				state = stateReady
			}

		case *RequestMessage:
			if state != stateReady {
				return fmt.Errorf("protocol error: request message in state %d", state)
			}
			// Requests are handled asynchronously
			go c.handleRequest(*msg)

		case *ResponseMessage:
			if state != stateReady {
				return fmt.Errorf("protocol error: response message in state %d", state)
			}
			c.handleResponse(*msg)

		case *DownloadProgressMessage:
			if state != stateReady {
				return fmt.Errorf("protocol error: response message in state %d", state)
			}
			c.receiver.DownloadProgress(c.id, msg.Folder, msg.Updates)

		case *PingMessage:
			if state != stateReady {
				return fmt.Errorf("protocol error: ping message in state %d", state)
			}
			// Nothing

		case *CloseMessage:
			return errors.New(msg.Reason)

		default:
			return fmt.Errorf("protocol error: %s: unknown message type %#x", c.id, hdr.msgType)
		}
	}
}

func (c *rawConnection) readMessage() (hdr header, msg encodable, err error) {
	hdrBuf := make([]byte, 8)
	_, err = io.ReadFull(c.cr, hdrBuf)
	if err != nil {
		return
	}

	hdr = decodeHeader(binary.BigEndian.Uint32(hdrBuf[:4]))
	msglen := int(binary.BigEndian.Uint32(hdrBuf[4:]))

	l.Debugf("read header %v (msglen=%d)", hdr, msglen)

	if msglen > MaxMessageLen {
		err = fmt.Errorf("message length %d exceeds maximum %d", msglen, MaxMessageLen)
		return
	}

	if hdr.version != 0 {
		err = fmt.Errorf("unknown protocol version 0x%x", hdr.version)
		return
	}

	// c.readerBuf contains a buffer we can reuse. But once we've unmarshalled
	// a message from the buffer we can't reuse it again as the unmarshalled
	// message refers to the contents of the buffer. The only case we a buffer
	// ends up in readerBuf for reuse is when the message is compressed, as we
	// then decompress into a new buffer instead.

	var msgBuf []byte
	if cap(c.readerBuf) >= msglen {
		// If we have a buffer ready in rdbuf we just use that.
		msgBuf = c.readerBuf[:msglen]
	} else {
		// Otherwise we allocate a new buffer.
		msgBuf = make([]byte, msglen)
	}

	_, err = io.ReadFull(c.cr, msgBuf)
	if err != nil {
		return
	}

	l.Debugf("read %d bytes", len(msgBuf))

	if hdr.compression && msglen > 0 {
		// We're going to decompress msgBuf into a different newly allocated
		// buffer, so keep msgBuf around for reuse on the next message.
		c.readerBuf = msgBuf

		msgBuf, err = lz4.Decode(nil, msgBuf)
		if err != nil {
			return
		}
		l.Debugf("decompressed to %d bytes", len(msgBuf))
	} else {
		c.readerBuf = nil
	}

	if shouldDebug() {
		if len(msgBuf) > 1024 {
			l.Debugf("message data:\n%s", hex.Dump(msgBuf[:1024]))
		} else {
			l.Debugf("message data:\n%s", hex.Dump(msgBuf))
		}
	}

	switch hdr.msgType {
	case messageTypeIndex, messageTypeIndexUpdate:
		msg = new(IndexMessage)

	case messageTypeRequest:
		msg = new(RequestMessage)

	case messageTypeResponse:
		msg = new(ResponseMessage)

	case messageTypePing:
		msg = new(PingMessage)

	case messageTypeClusterConfig:
		msg = new(ClusterConfigMessage)

	case messageTypeClose:
		msg = new(CloseMessage)

	case messageTypeDownloadProgress:
		msg = new(DownloadProgressMessage)

	default:
		err = fmt.Errorf("protocol error: %s: unknown message type %#x", c.id, hdr.msgType)
	}

	if msg != nil {
		// Unmarshal the actual type selected above, unless none was selected.
		err = msg.Unmarshal(msgBuf)
	}

	return
}

func (c *rawConnection) handleIndex(im IndexMessage) {
	l.Debugf("Index(%v, %v, %d file)", c.id, im.Folder, len(im.Files))
	c.receiver.Index(c.id, im.Folder, filterIndexMessageFiles(im.Files))
}

func (c *rawConnection) handleIndexUpdate(im IndexMessage) {
	l.Debugf("queueing IndexUpdate(%v, %v, %d files)", c.id, im.Folder, len(im.Files))
	c.receiver.IndexUpdate(c.id, im.Folder, filterIndexMessageFiles(im.Files))
}

func filterIndexMessageFiles(fs []FileInfo) []FileInfo {
	var out []FileInfo
	for i, f := range fs {
		switch f.Name {
		case "", ".", "..", "/": // A few obviously invalid filenames
			l.Infof("Dropping invalid filename %q from incoming index", f.Name)
			if out == nil {
				// Most incoming updates won't contain anything invalid, so we
				// delay the allocation and copy to output slice until we
				// really need to do it, then copy all the so var valid files
				// to it.
				out = make([]FileInfo, i, len(fs)-1)
				copy(out, fs)
			}
		default:
			if out != nil {
				out = append(out, f)
			}
		}
	}
	if out != nil {
		return out
	}
	return fs
}

func (c *rawConnection) handleRequest(req RequestMessage) {
	size := int(req.Size)
	usePool := size <= BlockSize

	var buf []byte
	var done chan struct{}

	if usePool {
		buf = c.pool.Get().([]byte)[:size]
		done = make(chan struct{})
	} else {
		buf = make([]byte, size)
	}

	err := c.receiver.Request(c.id, req.Folder, req.Name, int64(req.Offset), req.Hash, req.FromTemporary, buf)
	if err != nil {
		c.send(messageTypeResponse, &ResponseMessage{
			ID:   req.ID,
			Data: nil,
			Code: errorToCode(err),
		}, done)
	} else {
		c.send(messageTypeResponse, &ResponseMessage{
			ID:   req.ID,
			Data: buf,
			Code: errorToCode(err),
		}, done)
	}

	if usePool {
		<-done
		c.pool.Put(buf)
	}
}

func (c *rawConnection) handleResponse(resp ResponseMessage) {
	c.awaitingMut.Lock()
	if rc := c.awaiting[resp.ID]; rc != nil {
		delete(c.awaiting, resp.ID)
		rc <- asyncResult{resp.Data, codeToError(resp.Code)}
		close(rc)
	}
	c.awaitingMut.Unlock()
}

func (c *rawConnection) send(msgType MessageType, msg encodable, done chan struct{}) bool {
	hdr := header{
		version: 0,
		msgType: msgType,
	}

	select {
	case c.outbox <- hdrMsg{hdr, msg, done}:
		return true
	case <-c.closed:
		return false
	}
}

func (c *rawConnection) writerLoop() {
	var msgBuf = make([]byte, 8) // buffer for wire format message, kept and reused
	var uncBuf []byte            // buffer for uncompressed message, kept and reused
	for {
		var tempBuf []byte
		var err error

		select {
		case hm := <-c.outbox:
			if hm.msg != nil {
				// Uncompressed message in uncBuf
				msgLen := hm.msg.ProtoSize()
				if cap(uncBuf) >= msgLen {
					uncBuf = uncBuf[:msgLen]
				} else {
					uncBuf = make([]byte, msgLen)
				}
				_, err = hm.msg.MarshalTo(uncBuf)
				if hm.done != nil {
					close(hm.done)
				}
				if err != nil {
					c.close(err)
					return
				}

				compress := false
				switch c.compression {
				case CompressAlways:
					compress = true
				case CompressMetadata:
					compress = hm.hdr.msgType != messageTypeResponse
				}

				if compress && len(uncBuf) >= compressionThreshold {
					// Use compression for large messages
					hm.hdr.compression = true

					// Make sure we have enough space for the compressed message plus header in msgBug
					msgBuf = msgBuf[:cap(msgBuf)]
					if maxLen := lz4.CompressBound(len(uncBuf)) + 8; maxLen > len(msgBuf) {
						msgBuf = make([]byte, maxLen)
					}

					// Compressed is written to msgBuf, we keep tb for the length only
					tempBuf, err = lz4.Encode(msgBuf[8:], uncBuf)
					binary.BigEndian.PutUint32(msgBuf[4:8], uint32(len(tempBuf)))
					msgBuf = msgBuf[0 : len(tempBuf)+8]

					l.Debugf("write compressed message; %v (len=%d)", hm.hdr, len(tempBuf))
				} else {
					// No point in compressing very short messages
					hm.hdr.compression = false

					msgBuf = msgBuf[:cap(msgBuf)]
					if l := len(uncBuf) + 8; l > len(msgBuf) {
						msgBuf = make([]byte, l)
					}

					binary.BigEndian.PutUint32(msgBuf[4:8], uint32(len(uncBuf)))
					msgBuf = msgBuf[0 : len(uncBuf)+8]
					copy(msgBuf[8:], uncBuf)

					l.Debugf("write uncompressed message; %v (len=%d)", hm.hdr, len(uncBuf))
				}
			} else {
				l.Debugf("write empty message; %v", hm.hdr)
				binary.BigEndian.PutUint32(msgBuf[4:8], 0)
				msgBuf = msgBuf[:8]
			}

			binary.BigEndian.PutUint32(msgBuf[0:4], encodeHeader(hm.hdr))

			if err == nil {
				var n int
				n, err = c.cw.Write(msgBuf)
				l.Debugf("wrote %d bytes on the wire", n)
			}
			if err != nil {
				c.close(err)
				return
			}
		case <-c.closed:
			return
		}
	}
}

func (c *rawConnection) close(err error) {
	c.once.Do(func() {
		l.Debugln("close due to", err)
		close(c.closed)

		c.awaitingMut.Lock()
		for i, ch := range c.awaiting {
			if ch != nil {
				close(ch)
				delete(c.awaiting, i)
			}
		}
		c.awaitingMut.Unlock()

		go c.receiver.Close(c.id, err)
	})
}

// The pingSender makes sure that we've sent a message within the last
// PingSendInterval. If we already have something sent in the last
// PingSendInterval/2, we do nothing. Otherwise we send a ping message. This
// results in an effecting ping interval of somewhere between
// PingSendInterval/2 and PingSendInterval.
func (c *rawConnection) pingSender() {
	ticker := time.Tick(PingSendInterval / 2)

	for {
		select {
		case <-ticker:
			d := time.Since(c.cw.Last())
			if d < PingSendInterval/2 {
				l.Debugln(c.id, "ping skipped after wr", d)
				continue
			}

			l.Debugln(c.id, "ping -> after", d)
			c.ping()

		case <-c.closed:
			return
		}
	}
}

// The pingReceiver checks that we've received a message (any message will do,
// but we expect pings in the absence of other messages) within the last
// ReceiveTimeout. If not, we close the connection with an ErrTimeout.
func (c *rawConnection) pingReceiver() {
	ticker := time.Tick(ReceiveTimeout / 2)

	for {
		select {
		case <-ticker:
			d := time.Since(c.cr.Last())
			if d > ReceiveTimeout {
				l.Debugln(c.id, "ping timeout", d)
				c.close(ErrTimeout)
			}

			l.Debugln(c.id, "last read within", d)

		case <-c.closed:
			return
		}
	}
}

type Statistics struct {
	At            time.Time
	InBytesTotal  int64
	OutBytesTotal int64
}

func (c *rawConnection) Statistics() Statistics {
	return Statistics{
		At:            time.Now(),
		InBytesTotal:  c.cr.Tot(),
		OutBytesTotal: c.cw.Tot(),
	}
}
