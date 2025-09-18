package main

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"golang.org/x/net/context"
)

const (
	dirCacheTime   = 10 * time.Second
	statCacheTime  = 1 * time.Second
	attrValidTime  = 1 * time.Minute
	entryValidTime = 1 * time.Minute
)

type WebdavFS struct {
	Uid            uint32
	Gid            uint32
	Mode           uint32
	dirMode        os.FileMode
	fileMode       os.FileMode
	blockSize      uint32
	root           *Node
	CacheThreshold uint64
}

var FS *WebdavFS
var dav *DavClient

func attrSet(v fuse.SetattrValid, f fuse.SetattrValid) bool {
	return (v & f) > 0
}

func flagSet(v fuse.OpenFlags, f fuse.OpenFlags) bool {
	return (v & f) > 0
}

func getCMtime(c time.Time, m time.Time) (rc time.Time, rm time.Time) {
	rc = c
	rm = m
	if m.IsZero() {
		rm = time.Now()
	}
	if c.IsZero() {
		rc = rm
	}
	return
}

func NewFS(d *DavClient, config WebdavFS) *WebdavFS {

	if trace(T_FUSE) {
		tPrintf("NewFS %s", tJson(config))
	}

	dav = d
	FS = &config
	FS.root = rootNode

	if FS.Mode == 0 {
		FS.Mode = 0700
	}
	FS.Mode = FS.Mode & 0777
	FS.fileMode = os.FileMode(FS.Mode &^ uint32(0111))

	FS.dirMode = os.FileMode(FS.Mode)
	if FS.dirMode&0007 > 0 {
		FS.dirMode |= 0001
	}
	if FS.dirMode&0070 > 0 {
		FS.dirMode |= 0010
	}
	if FS.dirMode&0700 > 0 {
		FS.dirMode |= 0100
	}
	FS.dirMode |= os.ModeDir

	FS.blockSize = 4096
	if runtime.GOOS == "darwin" {
		// if we set this on osxfuse, _all_ I/O will
		// be limited to FS.blockSize bytes.
		FS.blockSize = 0
	}

	return FS
}

func (fs *WebdavFS) Root() (fs.Node, error) {
	return fs.root, nil
}

func (fs *WebdavFS) Statfs(ctx context.Context, req *fuse.StatfsRequest, resp *fuse.StatfsResponse) (err error) {
	if trace(T_FUSE) {
		tPrintf("%d Statfs()", req.Header.ID)
		defer func() {
			if err != nil {
				tPrintf("%d Statfs(): %v", req.Header.ID, err)
			} else {
				tPrintf("%d Statfs(): %v", req.Header.ID, resp)
			}
		}()
	}
	wanted := []string{"quota-available-bytes", "quota-used-bytes"}
	props, err := dav.PropFind("/", 0, wanted)
	if err != nil {
		return
	}

	negOne := int64(-1)
	total := uint64(negOne)
	free := uint64(negOne)

	if len(props) == 1 {
		spaceUsed, _ := strconv.ParseUint(props[0].SpaceUsed, 10, 64)
		spaceFree, _ := strconv.ParseUint(props[0].SpaceFree, 10, 64)
		if spaceUsed > 0 || spaceFree > 0 {
			used := (spaceUsed + 4095) / 4096
			free = (spaceFree + 4095) / 4096
			if free > 0 {
				total = used + free
			}
		}
	}

	data := fuse.StatfsResponse{
		Blocks:  total,
		Bfree:   free,
		Bavail:  free,
		Bsize:   4096,
		Frsize:  4096,
		Namelen: 255,
	}
	*resp = data
	return
}

func (nd *Node) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (ret fs.Node, err error) {
	if trace(T_FUSE) {
		tPrintf("%d Mkdir(%s)", req.Header.ID, req.Name)
		defer func() {
			if err != nil {
				tPrintf("%d Mkdir(%s): %v", req.Header.ID, req.Name, err)
			} else {
				tPrintf("%d Mkdir OK", req.Header.ID)
			}
		}()
	}
	nd.incMetaRefThenLock(req.Header.ID)
	path := joinPath(nd.getPath(), req.Name)
	nd.Unlock()
	err = dav.Mkcol(addSlash(path))
	nd.Lock()
	if err == nil {
		now := time.Now()
		nn := Dnode{
			Name:  req.Name,
			Mtime: now,
			Ctime: now,
			IsDir: true,
		}
		n := nd.addNode(nn, true)
		ret = n
	}
	nd.decMetaRef()
	nd.Unlock()
	return
}

func (nd *Node) Rename(ctx context.Context, req *fuse.RenameRequest, destDir fs.Node) (err error) {
	if trace(T_FUSE) {
		tPrintf("%d Rename(%s, %s)", req.Header.ID, req.OldName, req.NewName)
		defer func() {
			if err != nil {
				tPrintf("%d Rename(%s, %s): %v", req.Header.ID, req.OldName, req.NewName, err)
			} else {
				tPrintf("%d Rename OK", req.Header.ID)
			}
		}()
	}
	var lock1, lock2 *Node
	var oldPath, newPath string
	destNode := destDir.(*Node)
	first := true

	// Check if paths overlap. If so, only lock the
	// shortest path. If not, lock both.
	//
	// Need to do this in a loop, every time checking if this
	// condition still holds after both paths are locked.
	nd.Lock()
	for {
		srcDirPath := nd.getPath()
		dstDirPath := destNode.getPath()
		oldPath = joinPath(srcDirPath, req.OldName)
		newPath = joinPath(dstDirPath, req.NewName)

		var newLock1, newLock2 *Node
		if srcDirPath == dstDirPath {
			newLock1 = nd
		} else if strings.HasPrefix(srcDirPath, dstDirPath) {
			newLock1 = destNode
		} else if strings.HasPrefix(dstDirPath, srcDirPath) {
			newLock1 = nd
		} else {
			newLock1 = nd
			newLock2 = destNode
		}

		if !first {
			if lock1 == newLock1 && lock2 == newLock2 {
				break
			}
			lock1.decMetaRef()
			if lock2 != nil {
				lock2.decMetaRef()
			}
		}
		first = false

		lock1, lock2 = newLock1, newLock2
		lock1.incMetaRef(req.Header.ID)
		if lock2 != nil {
			lock2.incMetaRef(req.Header.ID)
		}
	}

	isDir := false
	node := nd.getNode(req.OldName)
	if node == nil {
		// don't have the source node cached- need to
		// find out if it's a dir or not, so stat.
		nd.Unlock()
		var dnode Dnode
		dnode, err = dav.Stat(oldPath)
		isDir = dnode.IsDir
	} else {
		isDir = node.IsDir
		// If the source is a regular file with open mem-handles or not yet uploaded, perform a
		// fast local rename and defer remote operations to handle flush/close.
		if !isDir && (len(node.OpenMem) > 0 || !node.RemoteExists) {
			// Drop any existing dest node locally to emulate overwrite semantics.
			if req.OldName != req.NewName {
				if destNode.getNode(req.NewName) != nil {
					destNode.delNode(req.NewName)
				}
				nd.moveNode(destNode, req.OldName, req.NewName)
			}
			nd.Unlock()
			return nil
		}
		nd.Unlock()
	}

	if err == nil {
		if isDir {
			oldPath = addSlash(oldPath)
			newPath = addSlash(newPath)
		}
		err = dav.Move(oldPath, newPath)
	}

	nd.Lock()
	if err == nil {
		nd.moveNode(destNode, req.OldName, req.NewName)
	}
	lock1.decMetaRef()
	if lock2 != nil {
		lock2.decMetaRef()
	}
	nd.Unlock()
	return
}

func (nd *Node) Remove(ctx context.Context, req *fuse.RemoveRequest) (err error) {
	if trace(T_FUSE) {
		tPrintf("%d Remove(%s)", req.Header.ID, req.Name)
		defer func() {
			if err != nil {
				tPrintf("%d Remove(%s): %v", req.Header.ID, req.Name, err)
			} else {
				tPrintf("%d Remove OK", req.Header.ID)
			}
		}()
	}
	nd.incMetaRefThenLock(req.Header.ID)
	path := joinPath(nd.getPath(), req.Name)
	// If the node exists only locally (not uploaded), remove locally and cancel pending uploads.
	if n := nd.getNode(req.Name); n != nil && !n.RemoteExists {
		// mark deleted and prevent memhandles from uploading
		n.Deleted = true
		for _, mh := range n.OpenMem {
			mh.mu.Lock()
			mh.dirty = false
			mh.mu.Unlock()
		}
		nd.delNode(req.Name)
		nd.decMetaRef()
		nd.Unlock()
		return nil
	}
	nd.Unlock()
	props, err := dav.PropFindWithRedirect(path, 1, nil)
	if err == nil {
		if len(props) != 1 {
			if req.Dir {
				err = fuse.Errno(syscall.ENOTEMPTY)
			} else {
				err = fuse.EIO
			}
		}
		if err == nil {
			isDir := false
			if props[0].ResourceType == "collection" {
				isDir = true
			}
			if req.Dir && !isDir {
				err = fuse.Errno(syscall.ENOTDIR)
			}
			if !req.Dir && isDir {
				err = fuse.Errno(syscall.EISDIR)
			}
		}
	}
	if err == nil {
		if req.Dir {
			path = addSlash(path)
		}
		err = dav.Delete(path)
	}
	nd.Lock()
	if err == nil {
		nd.delNode(req.Name)
	}
	nd.decMetaRef()
	nd.Unlock()
	return
}

func (nd *Node) Attr(ctx context.Context, attr *fuse.Attr) (err error) {
	// should not be called if Getattr exists.
	r := &fuse.GetattrRequest{}
	s := &fuse.GetattrResponse{}
	err = nd.Getattr(ctx, r, s)
	*attr = s.Attr
	return
}

func (nd *Node) Getattr(ctx context.Context, req *fuse.GetattrRequest, resp *fuse.GetattrResponse) (err error) {

	if trace(T_FUSE) {
		tPrintf("%d Getattr(%s)", req.Header.ID, nd.Name)
		defer func() {
			if err != nil {
				tPrintf("%d Getattr(%s): %v", req.Header.ID, nd.Name, err)
			} else {
				tPrintf("%d Getattr(%s): %v", req.Header.ID, nd.Name, tJson(resp))
			}
		}()
	}
	if nd.Deleted {
		err = fuse.Errno(syscall.ESTALE)
		return
	}

	nd.incIoRef(req.Header.ID)

	dnode := nd.Dnode
	if !nd.statInfoFresh() {
		path := nd.getPath()
		if nd.IsDir {
			path = addSlash(path)
		}
		dnode, err = dav.Stat(path)
		if err == nil {
			nd.statInfoTouch()
		}
	}

	if err == nil {

		// Sanity check.
		if nd.Name != "" && dnode.IsDir != nd.IsDir {
			nd.invalidateThisNode()
			err = fuse.Errno(syscall.ESTALE)
		} else {
			// All well, build fuse.Attr.
			nd.Dnode = dnode
			mode := FS.fileMode
			ctime, mtime := getCMtime(nd.Ctime, nd.Mtime)
			atime := nd.Atime
			if atime.IsZero() {
				atime = mtime
			}
			if nd.IsDir {
				mode = FS.dirMode
			}
			if nd.IsLink {
				mode = os.ModeSymlink | 0777
			}
			resp.Attr = fuse.Attr{
				Valid:     attrValidTime,
				Inode:     nd.Inode,
				Size:      nd.Size,
				Blocks:    (nd.Size + 511) / 512,
				Atime:     atime,
				Mtime:     mtime,
				Ctime:     ctime,
				Crtime:    ctime,
				Mode:      mode,
				Nlink:     1,
				Uid:       FS.Uid,
				Gid:       FS.Gid,
				BlockSize: FS.blockSize,
			}
		}
	}
	nd.decIoRef()
	return
}

func (nd *Node) Lookup(ctx context.Context, req *fuse.LookupRequest, resp *fuse.LookupResponse) (rn fs.Node, err error) {
	if trace(T_FUSE) {
		tPrintf("%d Lookup(%s)", req.Header.ID, req.Name)
		defer func() {
			if err != nil {
				tPrintf("%d Lookup(%s): %v", req.Header.ID, req.Name, err)
			} else {
				tPrintf("%d Lookup(%s): OK", req.Header.ID, req.Name)
			}
		}()
	}
	nd.incIoRef(req.Header.ID)
	defer nd.decIoRef()

	// do we have a recent entry available?
	nd.Lock()
	nn := nd.getNode(req.Name)
	valid := nn != nil && nn.statInfoFresh()
	nd.Unlock()
	if valid {
		rn = nn
		return
	}

	// need to call stat
	path := joinPath(nd.getPath(), req.Name)
	dnode, err := dav.Stat(path)

	if err == nil {
		node := nd.addNode(dnode, true)
		node.RemoteExists = true
		rn = node
	}
	nd.decIoRef()
	return
}

func (nd *Node) ReadDirAll(ctx context.Context) (dd []fuse.Dirent, err error) {
	if trace(T_FUSE) {
		tPrintf("- ReaddirAll(%s)", nd.Name)
		defer func() {
			if err != nil {
				tPrintf("- ReadDirAll(%s): %v", nd.Name, err)
			} else {
				tPrintf("- ReadDirAll(%s): %d entries", nd.Name, len(dd))
			}
		}()
	}
	nd.incIoRef(0)
	defer nd.decIoRef()

	path := nd.getPath()
	dirs, err := dav.Readdir(path, true)
	if err != nil {
		return
	}

	nd.Lock()
	defer nd.Unlock()

	seen := map[string]bool{}
	for _, d := range dirs {
		ino := nd.Inode
		if d.Name != "" && d.Name != "." {
			nn := nd.addNode(d, false)
			nn.RemoteExists = true
			ino = nn.Inode
		}

		tp := fuse.DT_File
		if d.IsDir {
			tp = fuse.DT_Dir
		}
		if d.IsLink {
			tp = fuse.DT_Link
		}
		dd = append(dd, fuse.Dirent{
			Name:  d.Name,
			Inode: ino,
			Type:  tp,
		})

		seen[d.Name] = true
	}
	for _, x := range nd.Child {
		if !seen[x.Name] {
			x.invalidateThisNode()
		}
	}
	return
}

func (nd *Node) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (node fs.Node, handle fs.Handle, err error) {
	nd.incMetaRefThenLock(req.Header.ID)
	path := nd.getPath()
	nd.Unlock()
	trunc := flagSet(req.Flags, fuse.OpenTruncate)
	read := req.Flags.IsReadWrite() || req.Flags.IsReadOnly()
	write := req.Flags.IsReadWrite() || req.Flags.IsWriteOnly()
	excl := flagSet(req.Flags, fuse.OpenExclusive)
	if trace(T_FUSE) {
		tPrintf("%d Create(%s): trunc=%v read=%v write=%v excl=%v",
			req.Header.ID, req.Name, trunc, read, write, excl)
		defer func() {
			if err != nil {
				tPrintf("%d Create(%s): %v", req.Header.ID, req.Name, err)
			} else {
				tPrintf("%d Create(%s): OK", req.Header.ID, req.Name)
			}
		}()
	}
	path = joinPath(path, req.Name)
	// Enforce exclusivity: if the target exists, fail.
	if excl {
		if _, statErr := dav.Stat(path); statErr == nil {
			err = fuse.EEXIST
		}
	}
	if err == nil {
		now := time.Now()
		dn := Dnode{Name: req.Name, IsDir: false, Size: 0, Mtime: now, Ctime: now}
		n := nd.addNode(dn, true)
		node = n
		// In-memory buffer starts empty (trunc semantics). Content will be uploaded on flush/close.
		mh := newMemHandle(n, []byte{}, req.Flags.IsReadWrite() || req.Flags.IsWriteOnly())
		handle = mh
	}
	nd.Lock()
	nd.decMetaRef()
	nd.Unlock()
	return
}

func (nd *Node) Forget() {
	if trace(T_FUSE) {
		tPrintf("Forget(%s)", nd.Name)
	}
	// XXX FIXME add some sanity checks here-
	// see if refcnt == 0, subdirs are gone
	nd.Lock()
	nd.forgetNode()
	nd.Unlock()
}

func (nd *Node) ftruncate(ctx context.Context, size uint64, id fuse.RequestID) (err error) {
	nd.incMetaRefThenLock(id)
	path := nd.getPath()
	// If there are open in-memory handles, resize their buffers and defer PUT to close/flush
	if len(nd.OpenMem) > 0 {
		for _, mh := range nd.OpenMem {
			mh.resize(size)
		}
		nd.Size = size
		nd.decMetaRef()
		nd.Unlock()
		return nil
	}
	nd.Unlock()
	if !dav.CanPutRange() {
		// Fallback: fetch, resize in-memory, and PUT full file.
		var data []byte
		if size == 0 {
			data = []byte{}
		} else {
			data, err = dav.Get(path)
			if err == nil {
				if uint64(len(data)) < size {
					nb := make([]byte, size)
					copy(nb, data)
					data = nb
				} else {
					data = data[:size]
				}
			}
		}
		if err == nil {
			_, err = dav.Put(path, data, false, false)
		}
	} else {
		if size == 0 {
			if nd.Size > 0 {
				_, err = dav.Put(path, []byte{}, false, false)
			}
		} else if size > nd.Size {
			_, err = dav.PutRange(path, []byte{0}, int64(size-1), false, false)
		} else if size != nd.Size {
			err = fuse.ERANGE
		}
	}
	nd.Lock()
	if err == nil {
		nd.Size = size
	}
	nd.decMetaRef()
	nd.Unlock()
	return
}

func (nd *Node) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) (err error) {
	if trace(T_FUSE) {
		tPrintf("%d Setattr(%s, %s)", req.Header.ID, nd.Name, tJson(req))
		defer func() {
			if err != nil {
				tPrintf("%d Setattr(%s): %v", req.Header.ID, nd.Name, err)
			} else {
				tPrintf("%d Setattr(%s): OK", req.Header.ID, nd.Name)
			}
		}()
	}
	if nd.Deleted {
		err = fuse.Errno(syscall.ESTALE)
		return
	}
	invalid := fuse.SetattrMode | fuse.SetattrUid | fuse.SetattrGid |
		fuse.SetattrBkuptime | fuse.SetattrCrtime | fuse.SetattrChgtime |
		fuse.SetattrFlags | fuse.SetattrHandle
	v := req.Valid
	if attrSet(v, invalid) {
		if trace(T_FUSE) {
			tPrintf("%d Setattr($s): invalid attributes (mode %d, invalid %d)",
				req.Header.ID, nd.Name, v, invalid)
		}
		return fuse.EPERM
	}

	if attrSet(v, fuse.SetattrSize) {
		err = nd.ftruncate(ctx, req.Size, req.Header.ID)
		if err != nil {
			return
		}
	}

	nd.Lock()
	defer nd.Unlock()

	// fake setting mtime if it is roughly unchanged.
	if attrSet(v, fuse.SetattrMtime) {
		if nd.LastStat.Add(time.Second).Before(time.Now()) ||
			req.Mtime.Before(nd.Mtime.Add(-500*time.Millisecond)) ||
			req.Mtime.After(nd.Mtime.Add(500*time.Millisecond)) {
			return fuse.EPERM
		}
	}
	// atime .. we allow it, but it's not saved.
	if attrSet(v, fuse.SetattrAtime) {
		nd.Atime = req.Atime
	}

	if attrSet(v, fuse.SetattrLockOwner) {
		// we ignore this for now, it's for mandatory locking
		// http://www.mail-archive.com/git-commits-head@vger.kernel.org/msg27852.html
	}

	mode := FS.fileMode
	if nd.IsDir {
		mode = FS.dirMode
	}
	if nd.IsLink {
		mode = os.ModeSymlink | 0777
	}
	ctime, mtime := getCMtime(nd.Ctime, nd.Mtime)
	atime := nd.Atime
	if atime.IsZero() {
		atime = mtime
	}
	attr := fuse.Attr{
		Valid:     attrValidTime,
		Inode:     nd.Inode,
		Size:      nd.Size,
		Blocks:    nd.Size / 512,
		Atime:     atime,
		Mtime:     mtime,
		Ctime:     ctime,
		Crtime:    ctime,
		Mode:      mode,
		Nlink:     1,
		Uid:       FS.Uid,
		Gid:       FS.Gid,
		BlockSize: 4096,
	}
	resp.Attr = attr
	return
}

func (nf *Node) Readlink(ctx context.Context, req *fuse.ReadlinkRequest) (string, error) {
	if !nf.IsLink {
		return "", fuse.Errno(syscall.EINVAL)
	}
	return nf.Target, nil
}

func (nf *Node) Fsync(ctx context.Context, req *fuse.FsyncRequest) (err error) {
	if trace(T_FUSE) {
		tPrintf("%d Fsync(%s)", req.Header.ID, nf.Name)
		defer func() {
			if err != nil {
				tPrintf("%d Fsync(%s): %v", req.Header.ID, nf.Name, err)
			}
		}()
	}
	if nf.Deleted {
		err = fuse.Errno(syscall.ESTALE)
		return
	}
	return nil
}

func (nf *Node) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) (err error) {
	if trace(T_FUSE) {
		tPrintf("%d Read(%s, %d, %d)", req.Header.ID, nf.Name, req.Offset, req.Size)
		defer func() {
			if err != nil {
				tPrintf("%d Read(%s): %v", req.Header.ID, nf.Name, err)
			} else {
				tPrintf("%d Read(%s): %d bytes", req.Header.ID, nf.Name, len(resp.Data))
			}
		}()
	}
	if nf.Deleted {
		err = fuse.Errno(syscall.ESTALE)
		return
	}
	nf.incIoRef(req.Header.ID)
	defer nf.decIoRef()
	nf.Lock()
	toRead := int64(nf.Size) - req.Offset
	nf.Unlock()
	if toRead <= 0 {
		resp.Data = []byte{}
		return
	}
	if toRead > int64(req.Size) {
		toRead = int64(req.Size)
	}
	path := nf.getPath()
	data, err := dav.GetRange(path, req.Offset, int(toRead))
	if err == nil {
		resp.Data = data
	}
	return
}

func (nf *Node) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) (err error) {
	if trace(T_FUSE) {
		tPrintf("%d Write(%s, %d, %d)", req.Header.ID, nf.Name, req.Offset, len(req.Data))
		defer func() {
			if err != nil {
				tPrintf("%d Write(%s): %v", req.Header.ID, nf.Name, err)
			} else {
				tPrintf("%d Write(%s): %d bytes", req.Header.ID, nf.Name, len(req.Data))
			}
		}()
	}
	if nf.Deleted {
		err = fuse.Errno(syscall.ESTALE)
		return
	}
	if len(req.Data) == 0 {
		resp.Size = 0
		return
	}
	nf.incIoRef(req.Header.ID)
	path := nf.getPath()
	_, err = dav.PutRange(path, req.Data, req.Offset, false, false)
	if err == nil {
		resp.Size = len(req.Data)
		sz := uint64(req.Offset) + uint64(len(req.Data))
		nf.Lock()
		if sz > nf.Size {
			nf.Size = sz
		}
		nf.Unlock()
	}
	nf.decIoRef()
	return
}

func (nf *Node) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (handle fs.Handle, err error) {
	trunc := flagSet(req.Flags, fuse.OpenTruncate)
	read := req.Flags.IsReadWrite() || req.Flags.IsReadOnly()
	write := req.Flags.IsReadWrite() || req.Flags.IsWriteOnly()

	if trace(T_FUSE) {
		tPrintf("%d Open(%s): trunc=%v read=%v write=%v", req.Header.ID, nf.Name, trunc, read, write)
		defer func() {
			if err != nil {
				tPrintf("%d Open(%s): %v", req.Header.ID, nf.Name, err)
			} else {
				tPrintf("%d Open(%s): OK", req.Header.ID, nf.Name)
			}
		}()
	}
	// Treat root or directory nodes as directories.
	if nf.IsDir || nf.Parent == nil {
		handle = nf
		return
	}

	nf.incIoRef(req.Header.ID)
	path := nf.getPath()

	// Decide whether to use RAM cache or ranged I/O based on file size and threshold
	useMem := true
	if dav.CanPutRange() && !trunc && FS.CacheThreshold > 0 {
		if dnode, stErr := dav.Stat(path); stErr == nil {
			nf.Lock()
			nf.Dnode = dnode
			nf.Size = dnode.Size
			nf.RemoteExists = true
			nf.statInfoTouch()
			nf.Unlock()
			if uint64(dnode.Size) > FS.CacheThreshold {
				useMem = false
			}
		}
	}

	if !useMem {
		// Non-cached handle using ranged I/O
		nf.decIoRef()
		handle = nf
		return
	}

	// Use RAM buffer: load full file content
	var data []byte
	if trunc {
		data = []byte{}
	} else {
		var gerr error
		data, gerr = dav.Get(path)
		if gerr != nil {
			// If not found or other error, start with empty buffer; it will be created on flush by writer.
			data = []byte{}
		} else {
			nf.Lock()
			nf.RemoteExists = true
			nf.Unlock()
		}
	}
	nf.decIoRef()
	mh := newMemHandle(nf, data, req.Flags.IsReadWrite() || req.Flags.IsWriteOnly())
	handle = mh
	return
}
