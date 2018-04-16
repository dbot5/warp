// Copyright 2009 The Go9p Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package warp9

// Write up to len(data) bytes starting from offset. Returns the
// number of bytes written, or an Error.
func (clnt *Clnt) Write(fid *Fid, data []byte, offset uint64) (int, W9Err) {
	if uint32(len(data)) > fid.Iounit {
		data = data[0:fid.Iounit]
	}

	tc := clnt.NewFcall()
	err := tc.packTwrite(fid.Fid, offset, uint32(len(data)), data)
	if err != Egood {
		return 0, err
	}

	rc, err := clnt.Rpc(tc)
	if err != Egood {
		return 0, err
	}

	return int(rc.Count), Egood
}

// Writes up to len(buf) bytes to a file. Returns the number of
// bytes written, or an Error.
func (file *File) Write(buf []byte) (int, W9Err) {
	n, err := file.WriteAt(buf, int64(file.offset))
	if err == Egood {
		file.offset += uint64(n)
	}

	return n, err
}

// Writes up to len(buf) bytes starting from offset. Returns the number
// of bytes written, or an Error.
func (file *File) WriteAt(buf []byte, offset int64) (int, W9Err) {
	return file.Fid.Clnt.Write(file.Fid, buf, uint64(offset))
}

// Writes exactly len(buf) bytes starting from offset. Returns the number of
// bytes written. If Error is returned the number of bytes can be less
// than len(buf).
func (file *File) Writen(buf []byte, offset uint64) (int, W9Err) {
	ret := 0
	for len(buf) > 0 {
		n, err := file.WriteAt(buf, int64(offset))
		if err != Egood {
			return ret, err
		}

		if n == 0 {
			break
		}

		buf = buf[n:]
		offset += uint64(n)
		ret += n
	}

	return ret, Egood
}
