// Copyright (C) 2014 The Protocol Authors.

package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"io/ioutil"
	"strings"
	"testing"
	"testing/quick"
	"time"
)

var (
	c0ID     = NewDeviceID([]byte{1})
	c1ID     = NewDeviceID([]byte{2})
	quickCfg = &quick.Config{}
)

func TestHeaderEncodeDecode(t *testing.T) {
	f := func(ver, id int, typ MessageType) bool {
		ver = int(uint(ver) % 16)
		id = int(uint(id) % 4096)
		typ = MessageType(uint(typ) % 256)
		h0 := header{version: ver, msgID: id, msgType: typ}
		h1 := decodeHeader(encodeHeader(h0))
		return h0 == h1
	}
	if err := quick.Check(f, nil); err != nil {
		t.Error(err)
	}
}

func TestHeaderMarshalUnmarshal(t *testing.T) {
	f := func(ver, id int, typ MessageType) bool {
		ver = int(uint(ver) % 16)
		id = int(uint(id) % 4096)
		typ = MessageType(uint(typ) % 256)
		buf := make([]byte, 4)

		h0 := header{version: ver, msgID: id, msgType: typ}
		h0.MarshalTo(buf)

		var h1 header
		h1.Unmarshal(buf)
		return h0 == h1
	}
	if err := quick.Check(f, nil); err != nil {
		t.Error(err)
	}
}

func TestHeaderLayout(t *testing.T) {
	var e, a uint32

	// Version are the first four bits
	e = 0xf0000000
	a = encodeHeader(header{version: 0xf})
	if a != e {
		t.Errorf("Header layout incorrect; %08x != %08x", a, e)
	}

	// Message ID are the following 12 bits
	e = 0x0fff0000
	a = encodeHeader(header{msgID: 0xfff})
	if a != e {
		t.Errorf("Header layout incorrect; %08x != %08x", a, e)
	}

	// Type are the last 8 bits before reserved
	e = 0x0000ff00
	a = encodeHeader(header{msgType: 0xff})
	if a != e {
		t.Errorf("Header layout incorrect; %08x != %08x", a, e)
	}
}

func TestPing(t *testing.T) {
	ar, aw := io.Pipe()
	br, bw := io.Pipe()

	c0 := NewConnection(c0ID, ar, bw, newTestModel(), "name", CompressAlways).(wireFormatConnection).Connection.(*rawConnection)
	c0.Start()
	c1 := NewConnection(c1ID, br, aw, newTestModel(), "name", CompressAlways).(wireFormatConnection).Connection.(*rawConnection)
	c1.Start()
	c0.ClusterConfig(ClusterConfigMessage{})
	c1.ClusterConfig(ClusterConfigMessage{})

	if ok := c0.ping(); !ok {
		t.Error("c0 ping failed")
	}
	if ok := c1.ping(); !ok {
		t.Error("c1 ping failed")
	}
}

func TestVersionErr(t *testing.T) {
	m0 := newTestModel()
	m1 := newTestModel()

	ar, aw := io.Pipe()
	br, bw := io.Pipe()

	c0 := NewConnection(c0ID, ar, bw, m0, "name", CompressAlways).(wireFormatConnection).Connection.(*rawConnection)
	c0.Start()
	c1 := NewConnection(c1ID, br, aw, m1, "name", CompressAlways)
	c1.Start()
	c0.ClusterConfig(ClusterConfigMessage{})
	c1.ClusterConfig(ClusterConfigMessage{})

	timeoutWriteHeader(c0.cw, header{
		version: 2, // higher than supported
		msgID:   0,
		msgType: messageTypeIndex,
	})

	if err := m1.closedError(); err == nil || !strings.Contains(err.Error(), "unknown protocol version") {
		t.Error("Connection should close due to unknown version, not", err)
	}
}

func TestTypeErr(t *testing.T) {
	m0 := newTestModel()
	m1 := newTestModel()

	ar, aw := io.Pipe()
	br, bw := io.Pipe()

	c0 := NewConnection(c0ID, ar, bw, m0, "name", CompressAlways).(wireFormatConnection).Connection.(*rawConnection)
	c0.Start()
	c1 := NewConnection(c1ID, br, aw, m1, "name", CompressAlways)
	c1.Start()
	c0.ClusterConfig(ClusterConfigMessage{})
	c1.ClusterConfig(ClusterConfigMessage{})

	timeoutWriteHeader(c0.cw, header{
		version: 0,
		msgID:   0,
		msgType: 42, // unknown type
	})

	if err := m1.closedError(); err == nil || !strings.Contains(err.Error(), "unknown message type") {
		t.Error("Connection should close due to unknown message type, not", err)
	}
}

func TestClose(t *testing.T) {
	m0 := newTestModel()
	m1 := newTestModel()

	ar, aw := io.Pipe()
	br, bw := io.Pipe()

	c0 := NewConnection(c0ID, ar, bw, m0, "name", CompressAlways).(wireFormatConnection).Connection.(*rawConnection)
	c0.Start()
	c1 := NewConnection(c1ID, br, aw, m1, "name", CompressAlways)
	c1.Start()
	c0.ClusterConfig(ClusterConfigMessage{})
	c1.ClusterConfig(ClusterConfigMessage{})

	c0.close(errors.New("manual close"))

	<-c0.closed
	if err := m0.closedError(); err == nil || !strings.Contains(err.Error(), "manual close") {
		t.Fatal("Connection should be closed")
	}

	// None of these should panic, some should return an error

	if c0.ping() {
		t.Error("Ping should not return true")
	}

	c0.Index("default", nil)
	c0.Index("default", nil)

	if _, err := c0.Request("default", "foo", 0, 0, nil, false); err == nil {
		t.Error("Request should return an error")
	}
}

func TestMarshalIndexMessage(t *testing.T) {
	if testing.Short() {
		quickCfg.MaxCount = 10
	}

	f := func(m1 IndexMessage) bool {
		if len(m1.Files) == 0 {
			m1.Files = nil
		}
		for i, f := range m1.Files {
			if len(f.Blocks) == 0 {
				m1.Files[i].Blocks = nil
			} else {
				for j := range f.Blocks {
					f.Blocks[j].Offset = 0
					if len(f.Blocks[j].Hash) == 0 {
						f.Blocks[j].Hash = nil
					}
				}
			}
			if len(f.Version.Counters) == 0 {
				m1.Files[i].Version.Counters = nil
			}
		}

		return testMarshal(t, "index", &m1, &IndexMessage{})
	}

	if err := quick.Check(f, quickCfg); err != nil {
		t.Error(err)
	}
}

func TestMarshalRequestMessage(t *testing.T) {
	if testing.Short() {
		quickCfg.MaxCount = 10
	}

	f := func(m1 RequestMessage) bool {
		if len(m1.Hash) == 0 {
			m1.Hash = nil
		}
		return testMarshal(t, "request", &m1, &RequestMessage{})
	}

	if err := quick.Check(f, quickCfg); err != nil {
		t.Error(err)
	}
}

func TestMarshalResponseMessage(t *testing.T) {
	if testing.Short() {
		quickCfg.MaxCount = 10
	}

	f := func(m1 ResponseMessage) bool {
		if len(m1.Data) == 0 {
			m1.Data = nil
		}
		return testMarshal(t, "response", &m1, &ResponseMessage{})
	}

	if err := quick.Check(f, quickCfg); err != nil {
		t.Error(err)
	}
}

func TestMarshalClusterConfigMessage(t *testing.T) {
	if testing.Short() {
		quickCfg.MaxCount = 10
	}

	f := func(m1 ClusterConfigMessage) bool {
		if len(m1.Folders) == 0 {
			m1.Folders = nil
		}
		for i := range m1.Folders {
			if len(m1.Folders[i].Devices) == 0 {
				m1.Folders[i].Devices = nil
			}
		}
		return testMarshal(t, "clusterconfig", &m1, &ClusterConfigMessage{})
	}

	if err := quick.Check(f, quickCfg); err != nil {
		t.Error(err)
	}
}

func TestMarshalCloseMessage(t *testing.T) {
	if testing.Short() {
		quickCfg.MaxCount = 10
	}

	f := func(m1 CloseMessage) bool {
		return testMarshal(t, "close", &m1, &CloseMessage{})
	}

	if err := quick.Check(f, quickCfg); err != nil {
		t.Error(err)
	}
}

type message interface {
	Marshal() ([]byte, error)
	Unmarshal([]byte) error
}

func testMarshal(t *testing.T, prefix string, m1, m2 message) bool {
	buf, err := m1.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	err = m2.Unmarshal(buf)
	if err != nil {
		t.Fatal(err)
	}

	bs1, _ := json.MarshalIndent(m1, "", "  ")
	bs2, _ := json.MarshalIndent(m2, "", "  ")
	if !bytes.Equal(bs1, bs2) {
		ioutil.WriteFile(prefix+"-1.txt", bs1, 0644)
		ioutil.WriteFile(prefix+"-2.txt", bs1, 0644)
		return false
	}

	return true
}

func timeoutWriteHeader(w io.Writer, hdr header) {
	// This tries to write a message header to w, but times out after a while.
	// This is useful because in testing, with a PipeWriter, it will block
	// forever if the other side isn't reading any more. On the other hand we
	// can't just "go" it into the background, because if the other side is
	// still there we should wait for the write to complete. Yay.

	var buf [8]byte // header and message length
	binary.BigEndian.PutUint32(buf[:], encodeHeader(hdr))
	binary.BigEndian.PutUint32(buf[4:], 0) // zero message length, explicitly

	done := make(chan struct{})
	go func() {
		w.Write(buf[:])
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
	}
}

func TestMarshalledIndexMessageSize(t *testing.T) {
	// We should be able to handle a 1 TiB file without
	// blowing the default max message size.

	if testing.Short() {
		t.Skip("this test requires a lot of memory")
		return
	}

	const (
		maxMessageSize = MaxMessageLen
		fileSize       = 1 << 40
		blockSize      = BlockSize
	)

	f := FileInfo{
		Name:        "a normal length file name withoout any weird stuff.txt",
		Type:        FileInfoTypeFile,
		Size:        fileSize,
		Permissions: 0666,
		Modified:    time.Now().Unix(),
		Version:     Vector{Counters: []Counter{{ID: 1 << 60, Value: 1}, {ID: 2 << 60, Value: 1}}},
		Blocks:      make([]BlockInfo, fileSize/blockSize),
	}

	for i := 0; i < fileSize/blockSize; i++ {
		f.Blocks[i].Offset = int64(i) * blockSize
		f.Blocks[i].Size = blockSize
		f.Blocks[i].Hash = []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 1, 2, 3, 4, 5, 6, 7, 8, 9, 20, 1, 2, 3, 4, 5, 6, 7, 8, 9, 30, 1, 2}
	}

	idx := IndexMessage{
		Folder: "some folder ID",
		Files:  []FileInfo{f},
	}

	msgSize := idx.ProtoSize()
	if msgSize > maxMessageSize {
		t.Errorf("Message size %d bytes is larger than max %d", msgSize, maxMessageSize)
	}
}
