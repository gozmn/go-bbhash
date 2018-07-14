// dbwriter.go -- Constant DB built on top of the BBHash MPH
//
// Author: Sudhi Herle <sudhi@herle.net>
//
// This software does not come with any express or implied
// warranty; it is provided "as is". No claim  is made to its
// suitability for any purpose.
package bbhash

import (
	"bufio"
	"crypto/sha512"
	"encoding/binary"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"runtime"
	"sync"

	"github.com/dchest/siphash"
	"github.com/opencoff/go-fasthash"
)

// Most data is serialized as big-endian integers. The exceptions are:
// Offset table:
//     This is mmap'd into the process and written as a little-endian uint64.
//     This is arguably an optimization -- most systems we work with are
//     little-endian. On big-endian systems, the DBReader code will convert
//     it on the fly to native-endian.

type DBWriter struct {
	fd *os.File

	// to detect duplicates
	keymap map[uint64]*record

	// list of unique keys
	keys []uint64

	// hash salt for hashing keys
	salt uint64

	// siphash key: just binary encoded salt
	saltkey []byte

	// running count of current offset within fd where we are writing
	// records
	off uint64

	bb *BBHash

	fntmp string
	fn    string
	frozen bool

	g   float64 // gamma
}

type header struct {
	magic [4]byte   // file magic
	resv00 uint32   // reserved - in future flags, algo choices etc.

	salt  uint64    // hash salt
	nkeys uint64    // number of keys in the system
	offtbl uint64	// file location where offset-table starts

	resv01 [4]uint64
}

type record struct {
	hash uint64

	key []byte
	val []byte

	// siphash of the key+val+offset+hash.
	csum uint64

	// offset where this record is written
	off uint64
}

// NewDBWriter prepares file 'fn' to hold a constant DB built using
// BBHash minimal perfect hash function. Once written, the DB is "frozen"
// and readers will open it using NewDBReader() to do constant time lookups
// of key to value.
func NewDBWriter(fn string) (*DBWriter, error) {
	tmp := fmt.Sprint("%s.tmp.%d", fn, rand64())

	fd, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return nil, err
	}

	w := &DBWriter{
		fd:      fd,
		keymap:  make(map[uint64]*record),
		keys:    make([]uint64, 0, 65536),
		salt:    rand64(),
		saltkey: make([]byte, 16),
		off:     64,
		fn:      fn,
		fntmp:   tmp,
	}

	// Leave some space for a header; we will fill this in when we
	// are done Freezing.
	var z [64]byte
	nw, err := fd.Write(z[:])
	if err != nil {
		return nil, w.error("can't write header: %s", err)
	}
	if nw != 64 {
		return nil, w.error("can't write blank-header: %s", err)
	}

	binary.BigEndian.PutUint64(w.saltkey[:8], w.salt)
	binary.BigEndian.PutUint64(w.saltkey[8:], ^w.salt)

	return w, nil
}

// AddTextFile adds contents from text file 'fn' where key and value are separated
// by one of the characters in 'delim'. Duplicates, Empty lines or lines with no value
// are skipped.
// Returns number of records added.
func (w *DBWriter) AddTextFile(fn string, delim string) (uint64, error) {
	fd, err := os.Open(fn)
	if err != nil {
		return 0, err
	}

	if len(delim) == 0 {
		delim = " \t"
	}

	defer fd.Close()

	rd := bufio.NewReader(fd)
	sc := bufio.NewScanner(rd)
	ch := make(chan *record, 10)

	// do I/O asynchronously
	go func(sc *bufio.Scanner, ch chan *record) {
		for sc.Scan() {
			s := strings.TrimSpace(sc.Text())
			if len(s) == 0 {
				continue
			}
			i := strings.IndexAny(s, delim)
			if i < 0 {
				continue
			}

			k := s[:i]
			v := s[i:]

			// ignore items that are too large
			if len(k) > 65535 || len(v) >= 4294967295 {
				continue
			}

			r := &record{
				key: []byte(k),
				val: []byte(v),
			}
			ch <- r
		}

		close(ch)
	}(sc, ch)

	return w.addFromChan(ch)
}

// AddCSVFile adds contents from CSV file 'fn'. If 'kwfield' and 'valfield' are
// non-negative, they indicate the field# of the key and value respectively; the
// default value for 'kwfield' & 'valfield' is 0 and 1 respectively.
// If 'comma' is not 0, the default CSV delimiter is ','.
// If 'comment' is not 0, then lines beginning with that rune are discarded.
// Records where the 'kwfield' and 'valfield' can't be evaluated are discarded.
// Returns number of records added.
func (w *DBWriter) AddCSVFile(fn string, comma, comment rune, kwfield, valfield int) (uint64, error) {
	if kwfield < 0 {
		kwfield = 0
	}

	if valfield < 0 {
		valfield = 1
	}

	var max int = valfield
	if kwfield > valfield {
		max = kwfield
	}

	max += 1

	fd, err := os.Open(fn)
	if err != nil {
		return 0, err
	}

	defer fd.Close()

	ch := make(chan *record, 10)
	cr := csv.NewReader(fd)
	cr.Comma = comma
	cr.Comment = comment
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	cr.ReuseRecord = true

	go func(cr *csv.Reader, ch chan *record) {
		for {
			v, err := cr.Read()
			if err != nil {
				break
			}

			if len(v) < max {
				continue
			}

			r := &record{
				key: []byte(v[kwfield]),
				val: []byte(v[valfield]),
			}
			ch <- r
		}
		close(ch)
	}(cr, ch)

	return w.addFromChan(ch)
}

// Freeze builds the minimal perfect hash, writes the DB and closes it.
// For very large key spaces, a higher 'g' value is recommended (2.5~4.0); otherwise,
// the Freeze() function will fail to generate an MPH.
func (w *DBWriter) Freeze(g float64) error {

	if w.frozen {
		return fmt.Errorf("%s: already frozen", w.fn)
	}

	bb, err := New(w.g, w.keys)
	if err != nil {
		return ErrMPHFail
	}

	offset := make([]uint64, len(w.keys))
	err = w.buildOffsets(bb, offset)
	if err != nil {
		return err
	}

	// We align the offset table to pagesize - so we can mmap it when we read it back.
	pgsz := uint64(os.Getpagesize())
	pgsz_m1 := pgsz - 1
	offtbl := w.off + pgsz_m1
	offtbl &= ^pgsz_m1

	var ehdr [64]byte

	// save info for building the file header.
	hdr := &header{
		magic:  [4]byte{'B', 'B', 'H', 'H'},
		salt:   w.salt,
		nkeys:  uint64(len(w.keys)),
		offtbl: offtbl,
	}
	/*
	hdr.magic[0] = 'B'
	hdr.magic[1] = 'B'
	hdr.magic[2] = 'H'
	hdr.magic[3] = 'H'
	*/

	hdr.encode(ehdr[:])

	w.fd.Seek(int64(offtbl), 0)

	// We won't encode concurrently and write to disk for two reasons:
	// 1. To make the I/O safe - we have to encode an entire worker's worth of offsets.
	//    This costs additional memory.
	// 2. There is no safe, portable way to do concurrent disk write without corrupting the
	//    file.

	var z [8]byte
	le := binary.LittleEndian

	// we calculate strong checksum for all data from this point on.
	h := sha512.New512_256()
	h.Write(ehdr[:])

	tee := io.MultiWriter(w.fd, h)
	for _, o := range offset {
		le.PutUint64(z[:], o)

		n, err := tee.Write(z[:])
		if err != nil {
			return err
		}
		if n != 8 {
			return fmt.Errorf("%s: partial write of offsets; exp %d saw %d", w.fntmp, 8, n)
		}
	}

	// We now encode the bbhash and write to disk.
	err = bb.MarshalBinary(tee)
	if err != nil {
		return err
	}

	// Trailer is the checksum of the meta-data.
	cksum := h.Sum(nil)
	n, err := w.fd.Write(cksum[:])
	if err != nil {
		return err
	}
	if n != sha512.Size256 {
		return fmt.Errorf("%s: partial write of checksum; exp %d saw %d", w.fntmp, sha512.Size256, n)
	}

	w.fd.Seek(0, 0)
	n, err = w.fd.Write(ehdr[:])
	if err != nil {
		return err
	}
	if n != 64 {
		return fmt.Errorf("%s: partial write of file header; exp %d saw %d", w.fntmp, 64, n)
	}

	w.frozen = true
	w.fd.Sync()
	w.fd.Close()

	err = os.Rename(w.fntmp, w.fn)
	if err != nil {
		return err
	}

	return nil
}


func (h *header) encode (b []byte) {
	be := binary.BigEndian
	copy(b[:4], h.magic[:])

	i := 8
	be.PutUint64(b[i:i+8], h.salt);  i += 8
	be.PutUint64(b[i:i+8], h.nkeys); i += 8
	be.PutUint64(b[i:i+8], h.offtbl)
}


// Abort stops the construction of the perfect hash db
func (w *DBWriter) Abort() {
	w.fd.Close()
	os.Remove(w.fntmp)
}


func (w *DBWriter) buildOffsets(bb *BBHash, offset []uint64) (error) {
	if len(w.keys) >= MinParallelKeys {
		return w.buildOffsetsConcurrent(bb, offset)
	}

	return w.buildOffsetSingle(bb, offset, w.keys)
}


func (w *DBWriter) buildOffsetSingle(bb *BBHash, offset, keys []uint64) error {
	for _, k := range keys {
		r := w.keymap[k]
		i := bb.Find(k)
		if i == 0 {
			return fmt.Errorf("%s: key <%s> with hash %#x can't be mapped", w.fn, string(r.key), k)
		}

		offset[i-1] = r.off
	}

	return nil
}


// concurrent construction of the offset table.
func (w *DBWriter) buildOffsetsConcurrent(bb *BBHash, offset []uint64) (error) {
	ncpu := runtime.NumCPU()

	n := len(w.keys) / ncpu
	r := len(w.keys) % ncpu

	errch := make(chan error, 1)

	var wg sync.WaitGroup
	wg.Add(ncpu)

	go func() {
		wg.Wait()
		close(errch)
	}()

	// shard keys across n cpus and find the MPH index for each key.
	for i := 0; i < ncpu; i++ {
		x := n * i
		y := x + n
		if i == (ncpu-1) {
			y += r
		}

		// XXX keymap may have to be locked for concurrent reads?
		go func(keys []uint64) {
			err := w.buildOffsetSingle(bb, offset, keys)
			if err != nil {
				errch <- err
			}
			wg.Done()
		}(w.keys[x:y])
	}

	// XXX What is the design pattern for returning errors from multiple workers?
	err := <- errch
	return err
}


// read partial records from the chan, complete them and write them to disk.
// Build up the internal tables as we go
func (w *DBWriter) addFromChan(ch chan *record) (uint64, error) {
	var n uint64
	buf := make([]byte, 0, 65536)
	for r := range ch {
		r.hash = fasthash.Hash64(w.salt, r.key)
		if _, ok := w.keymap[r.hash]; ok {
			continue
		}

		r.off = w.off
		r.csum = r.checksum(w.saltkey, w.off)

		b := r.encode(buf)
		nw, err := w.fd.Write(b)
		if err != nil {
			return 0, err
		}

		if nw != len(b) {
			return 0, fmt.Errorf("%s: partial write; exp %d saw %d", w.fntmp, len(b), nw)
		}

		w.keymap[r.hash] = r
		w.keys = append(w.keys, r.hash)
		w.off += uint64(nw)
		n++
	}

	return n, nil
}

// cleanup intermediate work and return an error instance
func (w *DBWriter) error(f string, v ...interface{}) error {
	w.fd.Close()
	os.Remove(w.fntmp)

	return fmt.Errorf(f, v...)
}

// Calculate a semi-string checksum on the important fields of the record
// at offset 'off'. In our implementation, we use siphash-24 (64-bit) as
// the strong checksum; and we use the offset as one of the items being
// protected.
func (r *record) checksum(key []byte, off uint64) uint64 {
	var b [8]byte

	be := binary.BigEndian

	h := siphash.New(key)
	h.Write(r.key)
	h.Write(r.val)

	be.PutUint64(b[:], off)
	h.Write(b[:])

	return h.Sum64()
}

// Provide a disk encoding of record r
func (r *record) encode(buf []byte) []byte {
	var b [2 + 4 + 8]byte

	klen := len(r.key)
	vlen := len(r.val)

	be := binary.BigEndian

	be.PutUint16(b[:2], uint16(klen))
	be.PutUint32(b[2:6], uint32(vlen))
	be.PutUint64(b[6:], r.csum)

	buf = append(buf, b[:]...)
	buf = append(buf, r.key...)
	buf = append(buf, r.val...)
	return buf
}


var ErrMPHFail = errors.New("failed to build MPH; gamma possibly small")

