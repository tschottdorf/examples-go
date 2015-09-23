// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Marc Berhault (marc@cockroachlabs.com)

package main

import (
	"encoding/json"
	"os"
	"syscall"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"golang.org/x/net/context"
)

var _ fs.Node = &Node{}               // Attr
var _ fs.NodeStringLookuper = &Node{} // Lookup
var _ fs.HandleReadDirAller = &Node{} // HandleReadDirAller
var _ fs.NodeMkdirer = &Node{}        // Mkdir
var _ fs.NodeCreater = &Node{}        // Create
var _ fs.NodeRemover = &Node{}        // Remove

// Node implements the Node interface.
type Node struct {
	cfs  CFS
	Name string
	ID   uint64
	// TODO(marc): switch to enum for other types.
	IsDir bool

	// For files only
	Data []byte
}

// toJSON returns the json-encoded string for this node.
func (n Node) toJSON() string {
	ret, err := json.Marshal(n)
	if err != nil {
		panic(err)
	}
	return string(ret)
}

// Attr fills attr with the standard metadata for the node.
func (n Node) Attr(_ context.Context, a *fuse.Attr) error {
	a.Inode = n.ID
	if n.IsDir {
		a.Mode = os.ModeDir | 0755
	} else {
		a.Mode = 0644
		a.Size = uint64(len(n.Data))
	}
	return nil
}

func (n Node) Getattr(ctx context.Context, _ *fuse.GetattrRequest, resp *fuse.GetattrResponse) error {
	return n.Attr(ctx, &resp.Attr)
}

// Lookup looks up a specific entry in the receiver,
// which must be a directory.  Lookup should return a Node
// corresponding to the entry.  If the name does not exist in
// the directory, Lookup should return ENOENT.
//
// Lookup need not to handle the names "." and "..".
func (n Node) Lookup(_ context.Context, name string) (fs.Node, error) {
	if !n.IsDir {
		return nil, fuse.Errno(syscall.ENOTDIR)
	}
	raw, err := n.cfs.lookup(n.ID, name)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return nil, fuse.ENOENT
		}
		return nil, err
	}
	node := &Node{}
	if err := json.Unmarshal([]byte(raw), node); err != nil {
		// TODO(marc): this defaults to EIO.
		return nil, err
	}
	node.cfs = n.cfs
	return node, nil
}

// ReadDirAll returns the list of child inodes.
func (n Node) ReadDirAll(_ context.Context) ([]fuse.Dirent, error) {
	if !n.IsDir {
		return nil, fuse.Errno(syscall.ENOTDIR)
	}
	return n.cfs.list(n.ID)
}

// Mkdir creates a directory in 'n'.
// We let the sql query fail if the directory already exists.
// TODO(marc): better handling of errors.
func (n Node) Mkdir(_ context.Context, req *fuse.MkdirRequest) (fs.Node, error) {
	if !n.IsDir {
		return nil, fuse.Errno(syscall.ENOTDIR)
	}
	if !req.Mode.IsDir() {
		return nil, fuse.Errno(syscall.ENOTDIR)
	}

	node := &Node{
		cfs:   n.cfs,
		ID:    n.cfs.newUniqueID(),
		Name:  req.Name,
		IsDir: true,
	}

	err := n.cfs.create(n.ID, *node)
	if err != nil {
		return nil, err
	}
	return node, nil
}

// Create creates a new file in the receiver directory.
func (n Node) Create(_ context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (
	fs.Node, fs.Handle, error) {
	if !n.IsDir {
		return nil, nil, fuse.Errno(syscall.ENOTDIR)
	}
	if req.Mode.IsDir() {
		return nil, nil, fuse.Errno(syscall.EISDIR)
	}

	node := &Node{
		cfs:   n.cfs,
		ID:    n.cfs.newUniqueID(),
		Name:  req.Name,
		IsDir: false,
	}

	err := n.cfs.create(n.ID, *node)
	if err != nil {
		return nil, nil, err
	}
	return node, node, nil
}

// Remove may be unlink or rmdir.
func (n Node) Remove(_ context.Context, req *fuse.RemoveRequest) error {
	if !n.IsDir {
		return fuse.Errno(syscall.ENOTDIR)
	}

	if req.Dir {
		// Rmdir.
		return n.cfs.remove(n.ID, req.Name, true /* checkChildren */)
	}
	// Unlink.
	return n.cfs.remove(n.ID, req.Name, false /* !checkChildren */)
}