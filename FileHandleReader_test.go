// Copyright (c) Microsoft. All rights reserved.
// Licensed under the MIT license. See LICENSE file in the project root for details.
package main

import (
	"bazil.org/fuse"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"io"
	"math/rand"
	"testing"
)

// Testing reading of an empty file
func TestEmptyFile(t *testing.T) {
	mockCtrl := gomock.NewController(t)
	hdfsReader := NewMockReadSeekCloser(mockCtrl)
	handle := createTestHandle(t, mockCtrl, hdfsReader)
	hdfsReader.whenReadReturn([]byte{}, io.EOF)
	handle.readAndVerify(t, 0, 1024, []byte{})
	hdfsReader.EXPECT().Close().Return(nil)
	handle.Release(nil, nil)
}

// Testing reading of a small "HelloWorld!" file using few Read() operations
func TestSmallFileSequentialRead(t *testing.T) {
	mockCtrl := gomock.NewController(t)
	hdfsReader := NewMockReadSeekCloser(mockCtrl)
	handle := createTestHandle(t, mockCtrl, hdfsReader)

	hdfsReader.whenReadReturn([]byte("Hel"), nil)
	hdfsReader.whenReadReturn([]byte("lo"), nil)
	handle.readAndVerify(t, 0, 5, []byte("Hello"))

	hdfsReader.whenReadReturn([]byte("World!"), nil)
	handle.readAndVerify(t, 5, 6, []byte("World!"))

	hdfsReader.whenReadReturn([]byte{}, io.EOF)
	handle.readAndVerify(t, 11, 1024, []byte{})

	hdfsReader.EXPECT().Close().Return(nil)
	handle.Release(nil, nil)
}

// If reads are reordered but not far away from each other
// this should not cause Seek() on the backend HDFS reader
func TestReoderedReadsDontCauseSeek(t *testing.T) {
	mockCtrl := gomock.NewController(t)
	hdfsReader := NewMockReadSeekCloser(mockCtrl)
	handle := createTestHandle(t, mockCtrl, hdfsReader)

	hdfsReader.whenReadReturn([]byte("He"), nil)
	handle.readAndVerify(t, 0, 2, []byte("He"))

	hdfsReader.whenReadReturn([]byte("ll"), nil)
	hdfsReader.whenReadReturn([]byte("oWorld!"), nil)
	handle.readAndVerify(t, 8, 3, []byte("ld!"))
	handle.readAndVerify(t, 2, 6, []byte("lloWor"))

	hdfsReader.EXPECT().Close().Return(nil)
	handle.Release(nil, nil)
}

// Seak()->Read()->Read()->Seek()->Read()
func TestSeekAndRead(t *testing.T) {
	mockCtrl := gomock.NewController(t)
	hdfsReader := NewMockReadSeekCloser(mockCtrl)
	handle := createTestHandle(t, mockCtrl, hdfsReader)

	hdfsReader.expectSeek(1000000)
	hdfsReader.whenReadReturn([]byte("foo"), nil)
	handle.readAndVerify(t, 1000000, 3, []byte("foo"))
	hdfsReader.whenReadReturn([]byte("bar"), nil)
	handle.readAndVerify(t, 1000003, 3, []byte("bar"))

	hdfsReader.expectSeek(2000000)
	hdfsReader.whenReadReturn([]byte("qux"), nil)
	hdfsReader.whenReadReturn([]byte("baz"), nil)
	handle.readAndVerify(t, 2000000, 6, []byte("quxbaz"))

	hdfsReader.EXPECT().Close().Return(nil)
	handle.Release(nil, nil)
}

// Testing of accessing a pseudo-random file of size 512K
// The goal of this test is to verify buffering and offset arithmetic
// For reads which are close to each other
func TestRandomAccess512K(t *testing.T) {
	RandomAccess(t, 1024*1024/2, 4096)
}

// Testing of accessing a pseudo-random file of size 5G
// This is to ensure that 64-bit offset handling works as expected
func TestRandomAccess5G(t *testing.T) {
	RandomAccess(t, 5*1024*1024*1024, 64*1024)
}

// Performing 1000 random reads on a virtual file with programmatically-generated content:
// The value of each byte is a simple function of its offset
func RandomAccess(t *testing.T, fileSize int64, maxRead int) {
	mockCtrl := gomock.NewController(t)
	r := rand.New(rand.NewSource(0))
	hdfsReader := &MockReadSeekCloserWithPseudoRandomContent{FileSize: fileSize, Rand: r}
	handle := createTestHandle(t, mockCtrl, hdfsReader)

	for iter := 0; iter < 1000; iter++ {
		// Generating a read of a random number of bytes from from random offset
		offset := r.Int63n(fileSize)
		size := r.Intn(maxRead) + 1
		// Computing maximum expected number of bytes which can be returned
		expectedMaxBytesRead := size
		expectedMinBytesRead := size
		if int64(expectedMinBytesRead) > fileSize-offset {
			expectedMinBytesRead = int(fileSize - offset)
		}

		// Executing read request
		resp := fuse.ReadResponse{Data: make([]byte, 0, size)}
		err := handle.Read(nil, &fuse.ReadRequest{Offset: offset, Size: size}, &resp)
		assert.Nil(t, err)
		assert.NotNil(t, resp.Data)
		actualBytesRead := len(resp.Data)
		assert.True(t, actualBytesRead <= expectedMaxBytesRead)
		assert.True(t, actualBytesRead >= expectedMinBytesRead)
		if expectedMaxBytesRead > 0 {
			assert.True(t, actualBytesRead != 0)
		}
		// verifying returned data
		for i := offset; i < offset+int64(actualBytesRead); i++ {
			if resp.Data[i-offset] != generateByteAtOffset(i) {
				t.Error("Invalid byte at offset ", i-offset)
				return
			}
		}
	}
	assert.False(t, hdfsReader.IsClosed)
	handle.Release(nil, nil)
	assert.True(t, hdfsReader.IsClosed)
}

///////////////// Test Helpers /////////////////////

// common setup for FileHandleReader testing
func createTestHandle(t *testing.T, mockCtrl *gomock.Controller, hdfsReader ReadSeekCloser) *FileHandle {
	hdfsAccessor := NewMockHdfsAccessor(mockCtrl)
	hdfsAccessor.EXPECT().Stat("/test.dat").Return(Attrs{Name: "test.dat"}, nil)
	hdfsAccessor.EXPECT().OpenRead("/test.dat").Return(hdfsReader, nil)
	fs, _ := NewFileSystem(hdfsAccessor, "/tmp/x", []string{"*"}, false, false, NewDefaultRetryPolicy(&MockClock{}), &MockClock{})
	root, _ := fs.Root()
	file, _ := root.(*Dir).Lookup(nil, "test.dat")
	h, _ := file.(*File).Open(nil, &fuse.OpenRequest{Flags: fuse.OpenReadOnly}, nil)
	return h.(*FileHandle)
}

// sets hdfsReader mock to respond on Read() request in a certain way
func (hdfsReader *MockReadSeekCloser) whenReadReturn(data []byte, err error) {
	hdfsReader.EXPECT().Read(gomock.Any()).Do(
		func(buf []byte) {
			copy(buf, data)
		}).Return(len(data), err)
}

// sets hdfsReader mock to respond on Read() request in a certain way
func (hdfsReader *MockReadSeekCloser) expectSeek(pos int64) {
	hdfsReader.EXPECT().Seek(pos).Return(nil)
}

// issue a Read() request to a handle and check returned data
func (handle *FileHandle) readAndVerify(t *testing.T, offset int64, size int, data []byte) {
	resp := fuse.ReadResponse{Data: make([]byte, 0, size)}
	err := handle.Read(nil, &fuse.ReadRequest{Offset: offset, Size: size}, &resp)
	assert.Nil(t, err)
	assert.NotNil(t, resp.Data)
	assert.Equal(t, len(data), len(resp.Data))
	assert.Equal(t, data, resp.Data)
}
