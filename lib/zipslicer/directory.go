//
// Copyright (c) SAS Institute Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package zipslicer

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"

	"github.com/kr/pretty"
)

type Directory struct {
	File   []*File
	Size   int64
	DirLoc int64
	r      io.ReaderAt
	end64  zip64End
	loc64  zip64Loc
	end    zipEndRecord
}

// Return the offset of the zip central directory
func FindDirectory(r io.ReaderAt, size int64) (int64, error) {
	pos := size - directoryEndLen - directory64LocLen
	var endb [directoryEndLen + directory64LocLen]byte
	if _, err := r.ReadAt(endb[:], pos); err != nil {
		return 0, err
	}
	re := bytes.NewReader(endb[:])
	var loc64 zip64Loc
	var end zipEndRecord
	binary.Read(re, binary.LittleEndian, &loc64)
	binary.Read(re, binary.LittleEndian, &end)
	if end.Signature != directoryEndSignature {
		return 0, errors.New("zip central directory not found")
	}
	if end.TotalCDCount == uint16Max || end.CDSize == uint32Max || end.CDOffset == uint32Max {
		if loc64.Signature != directory64LocSignature {
			return 0, errors.New("expected ZIP64 locator")
		}
		// ZIP64
		var end64b [directory64EndLen]byte
		if _, err := r.ReadAt(end64b[:], int64(loc64.Offset)); err != nil {
			return 0, err
		}
		var end64 zip64End
		binary.Read(bytes.NewReader(end64b[:]), binary.LittleEndian, &end64)
		if end64.Signature != directory64EndSignature {
			return 0, errors.New("zip central directory not found")
		}
		return int64(end64.CDOffset), nil
	}
	return int64(end.CDOffset), nil
}

// Read a zip from a ReaderAt, with a separate copy of the central directory
func ReadWithDirectory(r io.ReaderAt, size int64, cd []byte) (*Directory, error) {
	dirLoc := size - int64(len(cd))
	files := make([]*File, 0)
	for {
		if binary.LittleEndian.Uint32(cd) != directoryHeaderSignature {
			break
		}
		var hdr zipCentralDir
		binary.Read(bytes.NewReader(cd), binary.LittleEndian, &hdr)
		f := &File{
			CreatorVersion:   hdr.CreatorVersion,
			ReaderVersion:    hdr.ReaderVersion,
			Flags:            hdr.Flags,
			Method:           hdr.Method,
			ModifiedTime:     hdr.ModifiedTime,
			ModifiedDate:     hdr.ModifiedDate,
			CRC32:            hdr.CRC32,
			CompressedSize:   uint64(hdr.CompressedSize),
			UncompressedSize: uint64(hdr.UncompressedSize),
			InternalAttrs:    hdr.InternalAttrs,
			ExternalAttrs:    hdr.ExternalAttrs,
			Offset:           uint64(hdr.Offset),

			r:  r,
			rs: size,
		}
		f.raw = make([]byte, directoryHeaderLen+int(hdr.FilenameLen)+int(hdr.ExtraLen)+int(hdr.CommentLen))
		copy(f.raw, cd)
		cd = cd[directoryHeaderLen:]
		f.Name, cd = string(cd[:int(hdr.FilenameLen)]), cd[int(hdr.FilenameLen):]
		f.Extra, cd = cd[:int(hdr.ExtraLen)], cd[int(hdr.ExtraLen):]
		f.Comment, cd = cd[:int(hdr.CommentLen)], cd[int(hdr.CommentLen):]
		needUSize := f.UncompressedSize == uint32Max
		needCSize := f.CompressedSize == uint32Max
		needOffset := f.Offset == uint32Max
		extra := f.Extra
		for len(extra) >= 4 {
			tag := binary.LittleEndian.Uint16(extra[:2])
			size := binary.LittleEndian.Uint16(extra[2:4])
			if int(size) > len(extra)-4 {
				break
			}
			if tag == zip64ExtraID {
				e := extra[4 : 4+size]
				if needUSize && size >= 8 {
					f.UncompressedSize = binary.LittleEndian.Uint64(e)
					needUSize = false
				}
				if needCSize && size >= 16 {
					f.CompressedSize = binary.LittleEndian.Uint64(e[8:])
					needCSize = false
				}
				if needOffset && size >= 24 {
					f.Offset = binary.LittleEndian.Uint64(e[16:])
					needOffset = false
				}
				break
			}
			extra = extra[4+size:]
		}
		if needCSize || needOffset {
			return nil, errors.New("missing ZIP64 header")
		}
		files = append(files, f)
	}
	d := &Directory{
		File:   files,
		Size:   size,
		DirLoc: dirLoc,
		r:      r,
	}
	rd := bytes.NewReader(cd)
	switch binary.LittleEndian.Uint32(cd) {
	case directory64EndSignature:
		binary.Read(rd, binary.LittleEndian, &d.end64)
		binary.Read(rd, binary.LittleEndian, &d.loc64)
	case directoryEndSignature:
	default:
		return nil, errors.New("expected end record")
	}
	binary.Read(rd, binary.LittleEndian, &d.end)
	return d, nil
}

// Read a zip from a ReaderAt
func Read(r io.ReaderAt, size int64) (*Directory, error) {
	loc, err := FindDirectory(r, size)
	if err != nil {
		return nil, err
	}
	cd := make([]byte, size-loc)
	if _, err := r.ReadAt(cd, loc); err != nil {
		return nil, err
	}
	return ReadWithDirectory(r, size, cd)
}

// Read a zip from a stream, using a separate copy of the central directory.
// Contents must be read in zip order or an error will be raised.
func ReadStream(r io.Reader, size int64, cd []byte) (*Directory, error) {
	ra := &streamReaderAt{r: r}
	return ReadWithDirectory(ra, size, cd)
}

// Serialize a zip file with all of the files up to, but not including, the
// given index. The contents and central directory are written to separate
// writers, which may be the same writer.
func (d *Directory) Truncate(n int, body, dir io.Writer) error {
	if body != nil {
		for i := 0; i < n; i++ {
			f := d.File[i]
			fs, err := f.GetTotalSize()
			if err != nil {
				return err
			}
			if _, err := io.Copy(body, io.NewSectionReader(d.r, int64(f.Offset), fs)); err != nil {
				return err
			}
		}
	}
	cdOffset := d.File[n].Offset
	var size uint64
	for i := 0; i < n; i++ {
		blob, err := d.File[i].GetDirectoryHeader()
		if err != nil {
			return err
		}
		dir.Write(blob)
		size += uint64(len(blob))
	}
	end := d.end
	if d.end64.Signature != 0 {
		end64 := d.end64
		end64.DiskCDCount = uint64(n)
		end64.TotalCDCount = uint64(n)
		end64.CDSize = size
		end64.CDOffset = cdOffset
		binary.Write(dir, binary.LittleEndian, end64)
		loc := d.loc64
		loc.Offset = cdOffset + size
		binary.Write(dir, binary.LittleEndian, loc)
	} else {
		if cdOffset >= uint32Max || n >= uint16Max {
			return errors.New("file too big for 32-bit ZIP")
		}
		end.DiskCDCount = uint16(n)
		end.TotalCDCount = uint16(n)
		end.CDSize = uint32(size)
		end.CDOffset = uint32(cdOffset)
	}
	binary.Write(dir, binary.LittleEndian, end)
	return nil
}

// Serialize a zip central directory to file. The file entries will be written
// to wcd, and the end-of-directory markers will be written to weod.
//
// If forceZip64 is true then a ZIP64 end-of-directory marker will always be
// written; otherwise it is only done if ZIP64 features are required.
func (d *Directory) WriteDirectory(wcd, weod io.Writer, forceZip64 bool) error {
	buf := bufio.NewWriter(wcd)
	cdoff := d.DirLoc
	var count, size uint64
	minVersion := uint16(zip20)
	for _, f := range d.File {
		if f.ReaderVersion > minVersion {
			minVersion = f.ReaderVersion
		}
		blob, err := f.GetDirectoryHeader()
		if err != nil {
			return err
		}
		if _, err := buf.Write(blob); err != nil {
			return err
		}
		count++
		size += uint64(len(blob))
	}
	if wcd != weod {
		if err := buf.Flush(); err != nil {
			return err
		}
		buf.Reset(weod)
	}
	var end zipEndRecord
	if count >= uint16Max || size >= uint32Max || cdoff >= uint32Max || forceZip64 {
		minVersion = zip45
	}
	if minVersion == zip45 {
		end64off := cdoff + int64(size)
		end64 := zip64End{
			Signature:      directory64EndSignature,
			RecordSize:     directory64EndLen - 12,
			CreatorVersion: zip45,
			ReaderVersion:  minVersion,
			DiskCDCount:    count,
			TotalCDCount:   count,
			CDSize:         size,
			CDOffset:       uint64(cdoff),
		}
		pretty.Println(end64)
		if err := binary.Write(buf, binary.LittleEndian, end64); err != nil {
			return err
		}
		loc64 := zip64Loc{
			Signature: directory64LocSignature,
			Offset:    uint64(end64off),
			DiskCount: 1,
		}
		if err := binary.Write(buf, binary.LittleEndian, loc64); err != nil {
			return err
		}
		pretty.Println(loc64)
		end = zipEndRecord{
			Signature:    directoryEndSignature,
			DiskCDCount:  uint16Max,
			TotalCDCount: uint16Max,
			CDSize:       uint32Max,
			CDOffset:     uint32Max,
		}
	} else {
		end = zipEndRecord{
			Signature:    directoryEndSignature,
			DiskCDCount:  uint16(count),
			TotalCDCount: uint16(count),
			CDSize:       uint32(size),
			CDOffset:     uint32(cdoff),
		}
	}
	pretty.Println(end)
	if err := binary.Write(buf, binary.LittleEndian, end); err != nil {
		return err
	}
	return buf.Flush()
}

type streamReaderAt struct {
	r   io.Reader
	pos int64
}

func (r *streamReaderAt) ReadAt(d []byte, p int64) (int, error) {
	if p > r.pos {
		if _, err := io.CopyN(ioutil.Discard, r.r, p-r.pos); err != nil {
			return 0, err
		}
		r.pos = p
	} else if p < r.pos {
		return 0, fmt.Errorf("attempted to seek backwards: at %d, to %d", r.pos, p)
	}
	n, err := r.r.Read(d)
	r.pos += int64(n)
	return n, err
}

// Add a file to the central directory. Its contents are assumed to be already
// located after the last added file.
func (d *Directory) AddFile(f *File) (*File, error) {
	size, err := f.GetTotalSize()
	if err != nil {
		return nil, err
	}
	offset := uint64(d.DirLoc)
	if f.Offset != offset {
		f.raw = nil
	}
	f.Offset = offset
	d.DirLoc += size
	d.File = append(d.File, f)
	return f, nil
}

// Copy the contents of a file from another zip directory to the given writer
// and add it to this directory.
func (d *Directory) AddFileContents(f *File, w io.Writer) (*File, error) {
	lfh, err := f.GetLocalHeader()
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(lfh); err != nil {
		return nil, err
	}
	pos := int64(f.Offset) + int64(len(lfh))
	raw := io.NewSectionReader(f.r, pos, int64(f.CompressedSize))
	if _, err := io.Copy(w, raw); err != nil {
		return nil, err
	}
	dd, err := f.GetDataDescriptor()
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(dd); err != nil {
		return nil, err
	}
	return d.AddFile(f)
}
