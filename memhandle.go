package main

import (
	"sync"
	"time"

	"bazil.org/fuse"
	"golang.org/x/net/context"
)

// MemHandle provides a per-open-file in-RAM buffer for servers
// without PUT Range support. It loads the full file on open and
// flushes back with a single PUT on close if modified.
type MemHandle struct {
	n        *Node
	buf      []byte
	dirty    bool
	writable bool
	mu       sync.Mutex
	timer    *time.Timer
	closed   bool
	flushing bool
}

func newMemHandle(n *Node, initial []byte, writable bool) *MemHandle {
	mh := &MemHandle{n: n, buf: initial, dirty: false, writable: writable}
	// Update node size from buffer for consistency.
	n.Lock()
	n.Size = uint64(len(mh.buf))
	n.OpenMem = append(n.OpenMem, mh)
	n.Unlock()
	return mh
}

// Read reads from the in-memory buffer.
func (h *MemHandle) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.n.incIoRef(req.Header.ID)
	defer h.n.decIoRef()

	off := req.Offset
	if off >= int64(len(h.buf)) {
		resp.Data = []byte{}
		return nil
	}
	// compute slice bounds
	end := off + int64(req.Size)
	if end > int64(len(h.buf)) {
		end = int64(len(h.buf))
	}
	resp.Data = h.buf[off:end]
	return nil
}

// Write writes into the in-memory buffer and marks it dirty.
func (h *MemHandle) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	if !h.writable {
		return fuse.EPERM
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.n.incIoRef(req.Header.ID)
	defer h.n.decIoRef()

	// Ensure buffer is large enough
	need := int(req.Offset) + len(req.Data)
	if need > len(h.buf) {
		// grow and zero-fill new region
		nb := make([]byte, need)
		copy(nb, h.buf)
		h.buf = nb
	}
	copy(h.buf[int(req.Offset):int(req.Offset)+len(req.Data)], req.Data)
	h.dirty = true
	h.scheduleFlushLocked()
	resp.Size = len(req.Data)

	// Update node size
	h.n.Lock()
	if uint64(need) > h.n.Size {
		h.n.Size = uint64(need)
	}
	h.n.Unlock()
	return nil
}

// Release flushes the buffer back to the server on close if dirty.
func (h *MemHandle) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	h.mu.Lock()
	h.closed = true
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
	defer h.mu.Unlock()
	// If not dirty, nothing to do.
	if !h.dirty || h.n.Deleted {
		return nil
	}

	h.n.incIoRef(req.Header.ID)
	path := h.n.getPath()
	// send full PUT with entire buffer
	// If the file does not exist remotely, create it on first upload.
	_, err := dav.Put(path, h.buf, !h.n.RemoteExists, false)
	h.n.decIoRef()
	if err != nil {
		return err
	}

	// Refresh node metadata afterwards (best effort)
	dnode, err2 := dav.Stat(path)
	h.n.Lock()
	if err2 == nil {
		h.n.Dnode = dnode
		h.n.Mtime = dnode.Mtime
		h.n.Ctime = dnode.Ctime
		h.n.Size = dnode.Size
		h.n.RemoteExists = true
	} else {
		// at least touch mtime
		h.n.Mtime = time.Now()
	}
	// remove from open list
	for i, mh := range h.n.OpenMem {
		if mh == h {
			h.n.OpenMem = append(h.n.OpenMem[:i], h.n.OpenMem[i+1:]...)
			break
		}
	}
	h.n.Unlock()
	return nil
}

// Flush pushes data like Release but keeps the handle open.
func (h *MemHandle) Flush(ctx context.Context, req *fuse.FlushRequest) error {
	h.mu.Lock()
	if h.timer != nil {
		h.timer.Stop()
	}
	defer h.mu.Unlock()
	if !h.dirty {
		return nil
	}
	h.n.incIoRef(req.Header.ID)
	path := h.n.getPath()
	_, err := dav.Put(path, h.buf, !h.n.RemoteExists, false)
	h.n.decIoRef()
	if err != nil {
		return err
	}
	h.dirty = false
	h.n.Lock()
	h.n.RemoteExists = true
	h.n.Unlock()
	return nil
}

// Fsync also flushes to remote.
func (h *MemHandle) Fsync(ctx context.Context, req *fuse.FsyncRequest) error {
	h.mu.Lock()
	if h.timer != nil {
		h.timer.Stop()
	}
	defer h.mu.Unlock()
	if !h.dirty {
		return nil
	}
	h.n.incIoRef(req.Header.ID)
	path := h.n.getPath()
	_, err := dav.Put(path, h.buf, !h.n.RemoteExists, false)
	h.n.decIoRef()
	if err != nil {
		return err
	}
	h.dirty = false
	h.n.Lock()
	h.n.RemoteExists = true
	h.n.Unlock()
	return nil
}

// resize adjusts the in-memory buffer size.
func (h *MemHandle) resize(newSize uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cur := uint64(len(h.buf))
	if newSize == cur {
		return
	}
	if newSize < cur {
		h.buf = h.buf[:newSize]
	} else {
		nb := make([]byte, newSize)
		copy(nb, h.buf)
		h.buf = nb
	}
	h.dirty = true
	h.scheduleFlushLocked()
}

// scheduleFlushLocked (re)arms the inactivity timer. Caller must hold h.mu.
func (h *MemHandle) scheduleFlushLocked() {
	if h.closed {
		return
	}
	if h.timer == nil {
		h.timer = time.AfterFunc(10*time.Second, func() {
			h.inactivityFlush()
		})
	} else {
		h.timer.Reset(10 * time.Second)
	}
}

// inactivityFlush performs a flush after the inactivity window if needed.
func (h *MemHandle) inactivityFlush() {
	h.mu.Lock()
	if h.closed || !h.dirty || h.flushing {
		h.mu.Unlock()
		return
	}
	h.flushing = true
	data := make([]byte, len(h.buf))
	copy(data, h.buf)
	create := !h.n.RemoteExists
	// optimistic clear; if new writes happen, dirty will be set again.
	h.dirty = false
	h.mu.Unlock()

	path := h.n.getPath()
	// background flush; no specific RequestID
	_, err := dav.Put(path, data, create, false)

	h.mu.Lock()
	h.flushing = false
	if err != nil {
		// mark dirty again to retry later
		h.dirty = true
		// leave timer nil so a future write will rearm; or rearm now
		h.scheduleFlushLocked()
		h.mu.Unlock()
		return
	}
	// success: mark remote existence
	h.n.Lock()
	h.n.RemoteExists = true
	h.n.Unlock()
	h.mu.Unlock()
}

// ReadAll allows cat-like reads to fetch the full buffer efficiently.
func (h *MemHandle) ReadAll(ctx context.Context) ([]byte, error) {
	// Provide a copy to avoid exposing internal buffer to mutation
	h.mu.Lock()
	defer h.mu.Unlock()
	cp := make([]byte, len(h.buf))
	copy(cp, h.buf)
	return cp, nil
}
