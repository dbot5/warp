// Copyright 2009 The Go9p Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The clnt package provides definitions and functions used to implement
// a 9P2000 file client.
package warp9

import (
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
)

// The Clnt type represents a Warp9 client. The client is connected to
// a Warp9 object server and its methods can be used to access and manipulate
// the files exported by the server.
type Clnt struct {
	sync.Mutex
	Debuglevel int    // =0 don't print anything, >0 print Fcalls, >1 print raw packets
	Msize      uint32 // Maximum size of the 9P messages
	Root       *Fid   // Fid that points to the rood directory
	Id         string // Used when printing debug messages
	Log        *Logger

	conn     net.Conn
	tagpool  *pool
	fidpool  *pool
	reqout   chan *Req
	done     chan bool
	reqfirst *Req
	reqlast  *Req
	err      W9Err

	reqchan chan *Req
	tchan   chan *Fcall

	next, prev *Clnt
}

// A Fid type represents a file on the server. Fids are used for the
// low level methods that correspond directly to the 9P2000 message requests
type Fid struct {
	sync.Mutex
	Clnt   *Clnt // Client the fid belongs to
	Iounit uint32
	Qid           // The Qid description for the file
	Mode   uint8  // Open mode (one of O* values) (if file is open)
	Fid    uint32 // Fid number
	User          // The user the fid belongs to
	walked bool   // true if the fid points to a walked file on the server
}

// The file is similar to the Fid, but is used in the high-level client
// interface. We expose the Fid so that client code can use Remove
// on a fid, the same way a kernel can.
type File struct {
	Fid    *Fid
	offset uint64
}

type Req struct {
	sync.Mutex
	Clnt       *Clnt
	Tc         *Fcall
	Rc         *Fcall
	Err        W9Err
	Done       chan *Req
	tag        uint16
	prev, next *Req
	fid        *Fid
}

type ClntList struct {
	sync.Mutex
	clntList, clntLast *Clnt
}

var clnts *ClntList
var DefaultDebuglevel int
var DefaultLogger *Logger

func (clnt *Clnt) Rpcnb(r *Req) W9Err {
	var tag uint16

	if r.Tc.Type == Tversion {
		tag = NOTAG
	} else {
		tag = r.tag
	}

	r.Tc.SetTag(tag)
	clnt.Lock()
	if clnt.err != Egood {
		clnt.Unlock()
		return clnt.err
	}

	if clnt.reqlast != nil {
		clnt.reqlast.next = r
	} else {
		clnt.reqfirst = r
	}

	r.prev = clnt.reqlast
	clnt.reqlast = r
	clnt.Unlock()

	clnt.reqout <- r
	return Egood
}

func (clnt *Clnt) Rpc(tc *Fcall) (rc *Fcall, err W9Err) {
	r := clnt.ReqAlloc()
	r.Tc = tc
	r.Done = make(chan *Req)
	err = clnt.Rpcnb(r)
	if err != Egood {
		return
	}

	<-r.Done
	rc = r.Rc
	err = r.Err
	clnt.ReqFree(r)
	return
}

func (clnt *Clnt) recv() {
	var err W9Err
	var buf []byte

	err = Egood
	pos := 0
	for {
		// Connect can change the client Msize.
		clntmsize := int(atomic.LoadUint32(&clnt.Msize))
		if len(buf) < clntmsize {
			b := make([]byte, clntmsize*8)
			copy(b, buf[0:pos])
			buf = b
			b = nil
		}

		n, oerr := clnt.conn.Read(buf[pos:])
		if oerr != nil || n == 0 {
			err = Eio
			clnt.Lock()
			clnt.err = err
			clnt.Unlock()
			goto closed
		}

		pos += n
		for pos > 4 {
			sz, _ := gint32(buf)
			if pos < int(sz) {
				if len(buf) < int(sz) {
					b := make([]byte, atomic.LoadUint32(&clnt.Msize)*8)
					copy(b, buf[0:pos])
					buf = b
					b = nil
				}

				break
			}

			fc, err, fcsize := Unpack(buf)
			clnt.Lock()
			if err != Egood {
				clnt.err = err
				clnt.conn.Close()
				clnt.Unlock()
				goto closed
			}

			if clnt.Debuglevel > 0 {
				clnt.logFcall(fc)
				if clnt.Debuglevel&DbgPrintPackets != 0 {
					log.Println("}-}", clnt.Id, fmt.Sprint(fc.Pkt))
				}

				if clnt.Debuglevel&DbgPrintFcalls != 0 {
					log.Println("}}}", clnt.Id, fc.String())
				}
			}

			var r *Req = nil
			for r = clnt.reqfirst; r != nil; r = r.next {
				if r.Tc.Tag == fc.Tag {
					break
				}
			}

			if r == nil {
				clnt.err = Einval
				clnt.conn.Close()
				clnt.Unlock()
				goto closed
			}

			r.Rc = fc
			if r.prev != nil {
				r.prev.next = r.next
			} else {
				clnt.reqfirst = r.next
			}

			if r.next != nil {
				r.next.prev = r.prev
			} else {
				clnt.reqlast = r.prev
			}
			clnt.Unlock()

			if r.Tc.Type != r.Rc.Type-1 {
				if r.Rc.Type != Rerror {
					r.Err = Einval
					log.Println(fmt.Sprintf("TTT %v", r.Tc))
					log.Println(fmt.Sprintf("RRR %v", r.Rc))
				} else {
					if r.Err == Egood {
						r.Err = Einval
					}
				}
			}

			if r.Done != nil {
				r.Done <- r
			}

			pos -= fcsize
			buf = buf[fcsize:]
		}
	}

closed:
	clnt.done <- true

	/* send error to all pending requests */
	clnt.Lock()
	r := clnt.reqfirst
	clnt.reqfirst = nil
	clnt.reqlast = nil
	if err == Egood {
		err = clnt.err
	}
	clnt.Unlock()
	for ; r != nil; r = r.next {
		r.Err = err
		if r.Done != nil {
			r.Done <- r
		}
	}

	clnts.Lock()
	if clnt.prev != nil {
		clnt.prev.next = clnt.next
	} else {
		clnts.clntList = clnt.next
	}

	if clnt.next != nil {
		clnt.next.prev = clnt.prev
	} else {
		clnts.clntLast = clnt.prev
	}
	clnts.Unlock()

	if sop, ok := (interface{}(clnt)).(StatsOps); ok {
		sop.statsUnregister()
	}
}

func (clnt *Clnt) send() {
	for {
		select {
		case <-clnt.done:
			return

		case req := <-clnt.reqout:
			if clnt.Debuglevel > 0 {
				clnt.logFcall(req.Tc)
				if clnt.Debuglevel&DbgPrintPackets != 0 {
					log.Println("{-{", clnt.Id, fmt.Sprint(req.Tc.Pkt))
				}

				if clnt.Debuglevel&DbgPrintFcalls != 0 {
					log.Println("{{{", clnt.Id, req.Tc.String())
				}
			}

			for buf := req.Tc.Pkt; len(buf) > 0; {
				n, err := clnt.conn.Write(buf)
				if err != nil {
					/* just close the socket, will get signal on clnt.done */
					clnt.conn.Close()
					break
				}

				buf = buf[n:]
			}
		}
	}
}

// Creates and initializes a new Clnt object. Doesn't send any data
// on the wire.
func NewClnt(c net.Conn, msize uint32) *Clnt {
	clnt := new(Clnt)
	clnt.conn = c
	clnt.Msize = msize
	clnt.Debuglevel = DefaultDebuglevel
	clnt.Log = DefaultLogger
	clnt.Id = c.RemoteAddr().String() + ":"
	clnt.tagpool = newPool(uint32(NOTAG))
	clnt.fidpool = newPool(NOFID)
	clnt.reqout = make(chan *Req)
	clnt.done = make(chan bool)
	clnt.reqchan = make(chan *Req, 16)
	clnt.tchan = make(chan *Fcall, 16)
	go clnt.recv()
	go clnt.send()

	clnts.Lock()
	if clnts.clntLast != nil {
		clnts.clntLast.next = clnt
	} else {
		clnts.clntList = clnt
	}

	clnt.prev = clnts.clntLast
	clnts.clntLast = clnt
	clnts.Unlock()

	if sop, ok := (interface{}(clnt)).(StatsOps); ok {
		sop.statsRegister()
	}

	return clnt
}

// Establishes a new socket connection to the 9P server and creates
// a client object for it. Negotiates the dialect and msize for the
// connection. Returns a Clnt object, or Error.
func Connect(c net.Conn, msize uint32) (*Clnt, W9Err) {
	clnt := NewClnt(c, msize)
	ver := "9P2000"

	clntmsize := atomic.LoadUint32(&clnt.Msize)
	tc := NewFcall(clntmsize)
	err := tc.packTversion(clntmsize, ver)
	if err != Egood {
		return nil, err
	}

	rc, err := clnt.Rpc(tc)
	if err != Egood {
		return nil, err
	}

	if rc.Msize < atomic.LoadUint32(&clnt.Msize) {
		atomic.StoreUint32(&clnt.Msize, rc.Msize)
	}

	return clnt, Egood
}

// Creates a new Fid object for the client
func (clnt *Clnt) FidAlloc() *Fid {
	fid := new(Fid)
	fid.Fid = clnt.fidpool.getId()
	fid.Clnt = clnt

	return fid
}

func (clnt *Clnt) NewFcall() *Fcall {
	select {
	case tc := <-clnt.tchan:
		return tc
	default:
	}
	return NewFcall(atomic.LoadUint32(&clnt.Msize))
}

func (clnt *Clnt) FreeFcall(fc *Fcall) {
	if fc != nil && len(fc.Buf) >= int(atomic.LoadUint32(&clnt.Msize)) {
		select {
		case clnt.tchan <- fc:
			break
		default:
		}
	}
}

func (clnt *Clnt) ReqAlloc() *Req {
	var req *Req
	select {
	case req = <-clnt.reqchan:
		break
	default:
		req = new(Req)
		req.Clnt = clnt
		req.tag = uint16(clnt.tagpool.getId())
	}
	return req
}

func (clnt *Clnt) ReqFree(req *Req) {
	clnt.FreeFcall(req.Tc)
	req.Tc = nil
	req.Rc = nil
	req.Err = Egood
	req.Done = nil
	req.next = nil
	req.prev = nil

	select {
	case clnt.reqchan <- req:
		break
	default:
		clnt.tagpool.putId(uint32(req.tag))
	}
}

func (clnt *Clnt) logFcall(fc *Fcall) {
	if clnt.Debuglevel&DbgLogPackets != 0 {
		pkt := make([]byte, len(fc.Pkt))
		copy(pkt, fc.Pkt)
		clnt.Log.Log(pkt, clnt, DbgLogPackets)
	}

	if clnt.Debuglevel&DbgLogFcalls != 0 {
		f := new(Fcall)
		*f = *fc
		f.Pkt = nil
		clnt.Log.Log(f, clnt, DbgLogFcalls)
	}
}

// FidFile returns a File that represents the given Fid, initially at the given
// offset.
func FidFile(fid *Fid, offset uint64) *File {
	return &File{fid, offset}
}

func init() {
	clnts = new(ClntList)
	if sop, ok := (interface{}(clnts)).(StatsOps); ok {
		sop.statsRegister()
	}
}
