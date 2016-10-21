// Copyright (c) Microsoft. All rights reserved.
// Licensed under the MIT license. See LICENSE file in the project root for details.
package main

import (
	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"golang.org/x/net/context"
	"log"
	"os/user"
	"path"
	"sync"
	"time"
	"fmt"
)

type File struct {
	FileSystem *FileSystem // pointer to the FieSystem which owns this file
	Attrs      Attrs       // Cache of file attributes // TODO: implement TTL
	Parent     *Dir        // Pointer to the parent directory (allows computing fully-qualified paths on demand)

	activeHandles      []*FileHandle // list of opened file handles
	activeHandlesMutex sync.Mutex    // mutex for activeHandles
}

// Verify that *File implements necesary FUSE interfaces
var _ fs.Node = (*File)(nil)
var _ fs.NodeOpener = (*File)(nil)
var _ fs.NodeFsyncer = (*File)(nil)

// File is also a factory for ReadSeekCloser objects
var _ ReadSeekCloserFactory = (*File)(nil)

// Retunds absolute path of the file in HDFS namespace
func (this *File) AbsolutePath() string {
	return path.Join(this.Parent.AbsolutePath(), this.Attrs.Name)
}

// Responds to the FUSE file attribute request
func (this *File) Attr(ctx context.Context, a *fuse.Attr) error {
	if this.FileSystem.Clock.Now().After(this.Attrs.Expires) {
		err := this.Parent.LookupAttrs(this.Attrs.Name, &this.Attrs)
		if err != nil {
			return err
		}
	}
	return this.Attrs.Attr(a)
}

// Responds to the FUSE file open request (creates new file handle)
func (this *File) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	log.Printf("[%s] %v", this.AbsolutePath(), req.Flags)
	handle := NewFileHandle(this)
	if req.Flags.IsReadOnly() || req.Flags.IsReadWrite() {
		err := handle.EnableRead()
		if err != nil {
			return nil, err
		}
	}

	if req.Flags.IsWriteOnly() {
		// Enabling write only if opened in WriteOnly mode
		// In Read+Write scenario, write wills be enabled in lazy manner (on first write)
		newFile := req.Flags.IsWriteOnly() && (req.Flags&fuse.OpenAppend != fuse.OpenAppend)
		err := handle.EnableWrite(newFile)
		if err != nil {
			return nil, err
		}
	}

	this.AddHandle(handle)
	return handle, nil
}

// Opens file for reading
func (this *File) OpenRead() (ReadSeekCloser, error) {
	handle, err := this.Open(nil, &fuse.OpenRequest{Flags: fuse.OpenReadOnly}, nil)
	if err != nil {
		return nil, err
	}
	return NewFileHandleAsReadSeekCloser(handle.(*FileHandle)), nil
}

// Registers an opened file handle
func (this *File) AddHandle(handle *FileHandle) {
	this.activeHandlesMutex.Lock()
	defer this.activeHandlesMutex.Unlock()
	this.activeHandles = append(this.activeHandles, handle)
}

// Unregisters an opened file handle
func (this *File) RemoveHandle(handle *FileHandle) {
	this.activeHandlesMutex.Lock()
	defer this.activeHandlesMutex.Unlock()
	for i, h := range this.activeHandles {
		if h == handle {
			this.activeHandles = append(this.activeHandles[:i], this.activeHandles[i+1:]...)
			break
		}
	}
}

// Returns a snapshot of opened file handles
func (this *File) GetActiveHandles() []*FileHandle {
	this.activeHandlesMutex.Lock()
	defer this.activeHandlesMutex.Unlock()
	snapshot := make([]*FileHandle, len(this.activeHandles))
	copy(snapshot, this.activeHandles)
	return snapshot
}

// Responds to the FUSE Fsync request
func (this *File) Fsync(ctx context.Context, req *fuse.FsyncRequest) error {
	log.Printf("Dispatching fsync request to %d open handles", len(this.GetActiveHandles()))
	var retErr error
	for _, handle := range this.GetActiveHandles() {
		err := handle.Fsync(ctx, req)
		if err != nil {
			retErr = err
		}
	}
	return retErr
}

// Invalidates metadata cache, so next ls or stat gives up-to-date file attributes
func (this *File) InvalidateMetadataCache() {
	this.Attrs.Expires = this.FileSystem.Clock.Now().Add(-1 * time.Second)
}

// Responds on FUSE Chown/Chmod request
func (this *File) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	// Get the filepath, so chown/chmod in hdfs can work
	path := this.AbsolutePath()
	newusr, err := user.LookupId(fmt.Sprint(req.Uid))
	if err != nil {
		log.Printf("Error to get user information with request")
		return err
	}
	NewUser := newusr.Username
	newgrp, err := user.LookupGroupId(fmt.Sprint(newusr.Gid))
	if err != nil {
		log.Printf("Error to get group information with request")
		return err
	}
	NewGroup := newgrp.Name
	NewMode := req.Mode

	if NewMode != this.Attrs.Mode {
		log.Printf("Chmod [%s] to [%d]", path, NewMode)
		(func() {
			err = this.FileSystem.HdfsAccessor.Chmod(path, NewMode)
			// If error happens, exit the function, same as try
			if err != nil {
				return
			}
		})()

		if err != nil {
			// Catch the function err code
			log.Printf("Chmod failed with error: %v", err)
		} else {
			// Update the attrs in FUSE, only when HDFS sets attrs successfully
			this.Attrs.Mode = NewMode
		}
	}
	if req.Uid != this.Attrs.Uid || req.Gid != this.Attrs.Gid {
		log.Printf("Chown [%s] to [%s:%s]", path, NewUser, NewGroup)
		(func() {
			err = this.FileSystem.HdfsAccessor.Chown(path, NewUser, NewGroup)
			// If error happens, exit the function, same as try
			if err != nil {
				return
			}
		})()

		if err != nil {
			// Catch the function err code
			log.Printf("Chown failed with error: %v", err)
		} else {
			// Update the attrs in FUSE, only when HDFS sets attrs successfully 
			this.Attrs.Uid = req.Uid
			this.Attrs.Gid = req.Gid
		}
	}

	return err
}
