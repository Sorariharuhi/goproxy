package tunnel

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrBrokenPipe = errors.New("write in broken pipe")
)

const (
	ST_UNKNOWN  = 0x00
	ST_SYN_RECV = 0x01
	ST_SYN_SENT = 0x02
	ST_EST      = 0x03
	ST_FIN_RECV = 0x04
	ST_FIN_SENT = 0x06
)

type Addr struct {
	net.Addr
	streamid uint16
}

func (a *Addr) String() (s string) {
	return fmt.Sprintf("%s:%d", a.Addr.String(), a.streamid)
}

func RecvWithTimeout(ch chan uint32, t time.Duration) (errno uint32) {
	var ok bool
	ch_timeout := time.After(t)
	select {
	case errno, ok = <-ch:
		if !ok {
			return ERR_CLOSED
		}
	case <-ch_timeout:
		return ERR_TIMEOUT
	}
	return
}

// FIXME: Network, Address
type Conn struct {
	t        *Tunnel
	slock    sync.Mutex
	status   uint8
	streamid uint16
	ch_syn   chan uint32

	r_rest []byte
	rqueue *Queue
	wlock  sync.RWMutex
	window int32
	wev    *sync.Cond
}

func NewConn(t *Tunnel) (c *Conn) {
	c = &Conn{
		status: ST_UNKNOWN,
		t:      t,
		rqueue: NewQueue(),
		window: WINDOWSIZE,
		wev:    &sync.Cond{},
	}
	return
}

func (c *Conn) String() (s string) {
	return fmt.Sprintf("%s(%d)", c.t.String(), c.streamid)
}

func (c *Conn) Connect(network, address string) (err error) {
	c.ch_syn = make(chan uint32, 0)

	err = c.CheckAndSetStatus(ST_UNKNOWN, ST_SYN_SENT)
	if err != nil {
		return
	}

	syn := &Syn{
		Network: network,
		Address: address,
	}
	fsyn := NewFrame(MSG_SYN, c.streamid)
	err = fsyn.Marshal(syn)
	if err != nil {
		logger.Error(err.Error())
		c.Final()
		return
	}

	err = c.t.SendFrame(fsyn)
	if err != nil {
		logger.Error(err.Error())
		c.Final()
		return
	}

	errno := RecvWithTimeout(c.ch_syn, DIAL_TIMEOUT*time.Millisecond)

	if errno != ERR_NONE {
		logger.Errorf("remote connect %s failed for %d.", c.String(), errno)
		c.Final()
		return
	}
	err = c.CheckAndSetStatus(ST_SYN_SENT, ST_EST)
	if err != nil {
		return
	}
	logger.Infof("%s connected.", c.String())

	c.ch_syn = nil
	return
}

func (c *Conn) CheckAndSetStatus(old uint8, new uint8) (err error) {
	c.slock.Lock()
	defer c.slock.Unlock()
	if c.status != old {
		err = ErrState
		logger.Error(err.Error())
		return
	}
	c.status = new
	return
}

func (c *Conn) Read(data []byte) (n int, err error) {
	var v interface{}
	// c.rlock.Lock()
	// defer c.rlock.Unlock()

	target := data[:]
	for len(target) > 0 {
		if c.r_rest == nil {
			// when data isn't empty, reader should return.
			// when it is empty, reader should be blocked in here.
			v, err = c.rqueue.Pop(n == 0)
			if err == ErrQueueClosed {
				err = io.EOF
			}
			if err != nil {
				return
			}

			if v == nil {
				// what's this for?
				break
			}
			c.r_rest = v.([]byte)
		}

		size := copy(target, c.r_rest)
		target = target[size:]
		n += size

		if len(c.r_rest) > size {
			c.r_rest = c.r_rest[size:]
		} else {
			// take all data in rest
			c.r_rest = nil
		}
	}

	fwnd := NewFrame(MSG_WND, c.streamid)
	err = fwnd.Marshal(uint32(n))
	if err != nil {
		logger.Error(err.Error())
		return
	}

	err = c.t.SendFrame(fwnd)
	if err != nil {
		logger.Error(err.Error())
		return
	}
	return
}

func (c *Conn) Write(data []byte) (n int, err error) {
	for len(data) > 0 {
		size := uint16(len(data))
		// random size
		if size > 24*1024 {
			size = uint16(16*1024 + rand.Intn(16*1024))
		}

		err = c.writeSlice(data[:size])
		if err != nil {
			logger.Error(err.Error())
			return
		}
		logger.Debugf("%s send chunk size %d at %d.", c.String(), size, n)

		data = data[size:]
		n += int(size)
	}
	logger.Infof("%s sent %d bytes.", c.String(), n)
	return
}

func (c *Conn) writeSlice(data []byte) (err error) {
	c.wlock.Lock()
	defer c.wlock.Unlock()

	c.slock.Lock()
	if c.status != ST_EST {
		c.slock.Unlock()
		return ErrBrokenPipe
	}
	c.slock.Unlock()

	fdata := NewFrame(MSG_DATA, c.streamid)
	fdata.Data = data
	fdata.FrameHeader.Length = uint16(len(data))

	logger.Debugf("write data len: %d, window: %d", len(data), c.window)
	for c.window-int32(len(data)) < 0 {
		// just one goroutine could wait here.
		c.wev.Wait()
	}

	err = c.t.SendFrame(fdata)
	if err != nil {
		logger.Info(err.Error())
		return
	}

	c.window -= int32(len(data))
	return
}

func (c *Conn) Close() (err error) {
	logger.Infof("%s write close.", c.String())

	err = c.CloseWrite()

	c.slock.Lock()
	defer c.slock.Unlock()
	switch c.status {
	case ST_EST:
		c.status = ST_FIN_SENT
	case ST_FIN_RECV:
		c.status = ST_UNKNOWN
		c.Final()
	default:
		return ErrState
	}
	return
}

func (c *Conn) Final() {
	err := c.t.CloseFiber(c.streamid)
	if err != nil {
		logger.Error(err.Error())
		return
	}

	logger.Noticef("%s final.", c.String())
	return
}

func (c *Conn) CloseWrite() (err error) {
	c.wlock.Lock()
	defer c.wlock.Unlock()

	ffin := NewFrame(MSG_FIN, c.streamid)
	err = c.t.SendFrame(ffin)
	if err != nil {
		logger.Info(err.Error())
		return
	}
	return
}

func (c *Conn) CloseRead() (err error) {
	c.rqueue.Close()

	c.slock.Lock()
	defer c.slock.Unlock()
	switch c.status {
	case ST_EST:
		c.status = ST_FIN_RECV
	case ST_FIN_SENT:
		c.status = ST_UNKNOWN
		c.Final()
	default:
		return ErrState
	}
	return
}

func (c *Conn) LocalAddr() net.Addr {
	return &Addr{
		c.t.LocalAddr(),
		c.streamid,
	}
}

func (c *Conn) RemoteAddr() net.Addr {
	return &Addr{
		c.t.RemoteAddr(),
		c.streamid,
	}
}

func (c *Conn) SetDeadline(t time.Time) error {
	return nil
}

func (c *Conn) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *Conn) SetWriteDeadline(t time.Time) error {
	return nil
}

func (c *Conn) SendFrame(f *Frame) (err error) {
	switch f.FrameHeader.Type {
	default:
		err = ErrUnexpectedPkg
		logger.Error(err.Error())
		err = c.Close()
		if err != nil {
			logger.Error(err.Error())
		}
	case MSG_RESULT:
		c.slock.Lock()
		if c.status != ST_SYN_SENT {
			c.slock.Unlock()
			err = ErrState
			logger.Error(err.Error())
			return
		}
		c.slock.Unlock()

		var errno uint32
		err = f.Unmarshal(&errno)
		if err != nil {
			logger.Error(err.Error())
			return
		}

		select {
		case c.ch_syn <- errno:
		default:
		}
	case MSG_DATA:
		err = c.rqueue.Push(f.Data)
		switch err {
		default:
			return
		case ErrQueueClosed:
			// Drop data here
			err = nil
		case nil:
		}
		logger.Debugf("%s recved %d bytes.", c.String(), len(f.Data))
	case MSG_WND:
		var window Wnd
		err = f.Unmarshal(&window)
		if err != nil {
			return
		}

		c.wlock.Lock()
		c.window += int32(window)
		c.wlock.Unlock()
		c.wev.Signal()
		logger.Debugf("%s remote readed %d, write buffer size: %d.",
			c.String(), window, atomic.LoadInt32(&c.window))
	case MSG_FIN:
		logger.Infof("%s read close.", c.String())
		c.CloseRead()
	}
	return
}

func (c *Conn) CloseFiber(streamid uint16) (err error) {
	panic("why?")
	return
}
