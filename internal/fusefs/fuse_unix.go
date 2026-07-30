//go:build !windows

package fusefs

import (
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/jacobsa/fuse"
	"github.com/jacobsa/fuse/fuseops"
	"github.com/jacobsa/fuse/fuseutil"
)

// MountedFS matches tigrisfs's interface for mount lifecycle.
type MountedFS interface {
	Join(ctx context.Context) error
	Unmount() error
}

// ArtifactFuse is the FUSE adapter following the tigrisfs GoofysFuse pattern:
// embed NotImplementedFileSystem + core state, thin operation wrappers.
type ArtifactFuse struct {
	fuseutil.NotImplementedFileSystem
	repo           model.RepoConfig
	resolver       *Resolver
	engine         *Engine
	gitfileContent []byte // synthesized .git gitfile, computed once

	mu           sync.RWMutex
	handleOps    sync.RWMutex
	inodes       map[fuseops.InodeID]*InodeRef
	pathToInode  map[string]fuseops.InodeID
	nextInodeID  fuseops.InodeID
	dirHandles   map[fuseops.HandleID]*DirHandle
	fileHandles  map[fuseops.HandleID]*FileHandle
	nextHandleID fuseops.HandleID
}

type InodeRef struct {
	ID       fuseops.InodeID
	Path     string
	Type     string // file, dir, symlink
	Mode     uint32
	Gen      int64
	Refcnt   int64
	IsRoot   bool
	Overlay  bool
	Stale    bool
	Detached bool
}

type DirHandle struct {
	inode      *InodeRef
	gen        int64
	commitTime int64
	entries    []ReaddirEntry
}

type FileHandle struct {
	mu              sync.Mutex
	inode           *InodeRef
	path            string
	cacheFile       *os.File
	cacheGeneration int64
	invalidateSeq   uint64
	detached        bool
}

// ReaddirEntry holds child metadata, avoiding per-child Getattr or snapshot lookups.
type ReaddirEntry struct {
	Name        string
	Type        string // file, dir, symlink
	Mode        uint32
	ObjectOID   string
	SizeState   string
	SizeBytes   int64
	FromOverlay bool
	MtimeUnixNs int64
	CtimeUnixNs int64
}

func (e ReaddirEntry) direntType() fuseutil.DirentType {
	switch e.Type {
	case "dir":
		return fuseutil.DT_Directory
	case "symlink":
		return fuseutil.DT_Link
	default:
		return fuseutil.DT_File
	}
}

func NewArtifactFuse(repo model.RepoConfig, resolver *Resolver, engine *Engine) *ArtifactFuse {
	fs := &ArtifactFuse{
		repo:           repo,
		resolver:       resolver,
		engine:         engine,
		gitfileContent: fmt.Appendf(nil, "gitdir: %s\n", repo.GitDir),
		inodes:         make(map[fuseops.InodeID]*InodeRef),
		pathToInode:    make(map[string]fuseops.InodeID),
		nextInodeID:    fuseops.RootInodeID + 1,
		dirHandles:     make(map[fuseops.HandleID]*DirHandle),
		fileHandles:    make(map[fuseops.HandleID]*FileHandle),
		nextHandleID:   1,
	}
	root := &InodeRef{ID: fuseops.RootInodeID, Path: ".", Type: "dir", Mode: 0o755, Refcnt: 1, IsRoot: true}
	fs.inodes[fuseops.RootInodeID] = root
	fs.pathToInode["."] = fuseops.RootInodeID
	return fs
}

func (fs *ArtifactFuse) allocInode(path, typ string, mode uint32, gen int64) *InodeRef {
	// Caller must hold fs.mu write lock.
	if id, ok := fs.pathToInode[path]; ok {
		if ref, ok := fs.inodes[id]; ok {
			ref.Refcnt++
			return ref
		}
	}
	id := fs.nextInodeID
	fs.nextInodeID++
	ref := &InodeRef{ID: id, Path: path, Type: typ, Mode: mode, Gen: gen, Refcnt: 1}
	fs.inodes[id] = ref
	fs.pathToInode[path] = id
	return ref
}

func (fs *ArtifactFuse) requireInode(id fuseops.InodeID, missing error) (*InodeRef, error) {
	fs.mu.RLock()
	ref := fs.inodes[id]
	if ref == nil || ref.Stale {
		fs.mu.RUnlock()
		return nil, missing
	}
	snapshot := *ref
	fs.mu.RUnlock()
	return &snapshot, nil
}

func (fs *ArtifactFuse) dropInodeLookup(id fuseops.InodeID) {
	fs.mu.Lock()
	ref, ok := fs.inodes[id]
	if ok {
		ref.Refcnt--
		if ref.Refcnt <= 0 && !ref.IsRoot {
			delete(fs.inodes, id)
			if fs.pathToInode[ref.Path] == id {
				delete(fs.pathToInode, ref.Path)
			}
		}
	}
	fs.mu.Unlock()
}

func (fs *ArtifactFuse) moveInodePath(oldPath, newPath string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	type inodeMove struct {
		id      fuseops.InodeID
		oldPath string
		newPath string
	}
	var moves []inodeMove
	movingIDs := map[fuseops.InodeID]bool{}
	for path, id := range fs.pathToInode {
		if samePathOrDescendant(path, oldPath) {
			moves = append(moves, inodeMove{
				id:      id,
				oldPath: path,
				newPath: newPath + strings.TrimPrefix(path, oldPath),
			})
			movingIDs[id] = true
		}
	}
	for path, id := range fs.pathToInode {
		if samePathOrDescendant(path, newPath) && !movingIDs[id] {
			if replaced := fs.inodes[id]; replaced != nil {
				if replaced.Type == "dir" {
					replaced.Detached = true
				} else {
					replaced.Stale = true
				}
			}
			delete(fs.pathToInode, path)
		}
	}
	for _, move := range moves {
		delete(fs.pathToInode, move.oldPath)
	}
	for _, move := range moves {
		if ref := fs.inodes[move.id]; ref != nil {
			ref.Path = move.newPath
			fs.pathToInode[move.newPath] = move.id
		}
	}
	for _, dh := range fs.dirHandles {
		if samePathOrDescendant(dh.inode.Path, oldPath) {
			dh.inode.Path = newPath + strings.TrimPrefix(dh.inode.Path, oldPath)
		}
	}
}

func samePathOrDescendant(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+"/")
}

func (fs *ArtifactFuse) childPath(parentID fuseops.InodeID, name string) (*InodeRef, string, error) {
	parent, err := fs.requireInode(parentID, syscall.ENOENT)
	if err != nil {
		return nil, "", err
	}
	if parent.Detached {
		return nil, "", syscall.ENOENT
	}
	return parent, cleanChildPath(parent.Path, name), nil
}

func (fs *ArtifactFuse) dirHandle(handleID fuseops.HandleID) (*DirHandle, error) {
	fs.mu.RLock()
	dh := fs.dirHandles[handleID]
	fs.mu.RUnlock()
	if dh == nil {
		return nil, syscall.EBADF
	}
	return dh, nil
}

func (fs *ArtifactFuse) fileHandle(handleID fuseops.HandleID) (*FileHandle, error) {
	fs.mu.RLock()
	fh := fs.fileHandles[handleID]
	fs.mu.RUnlock()
	if fh == nil {
		return nil, syscall.EBADF
	}
	return fh, nil
}

func (fs *ArtifactFuse) closeCachedFilesForPath(path string) {
	fs.mu.RLock()
	var handles []*FileHandle
	for _, fh := range fs.fileHandles {
		fh.mu.Lock()
		matches := fh.path == path
		fh.mu.Unlock()
		if matches {
			handles = append(handles, fh)
		}
	}
	fs.mu.RUnlock()
	for _, fh := range handles {
		fh.closeCachedFile()
	}
}

func (fs *ArtifactFuse) pinOpenHandles(path string) error {
	ov, ok, err := fs.engine.Overlay.Lookup(context.Background(), path)
	if err != nil || !ok || ov.IsDeleted() || ov.BackingPath == "" {
		return err
	}
	fs.mu.RLock()
	var handles []*FileHandle
	for _, fh := range fs.fileHandles {
		fh.mu.Lock()
		matches := fh.path == path
		fh.mu.Unlock()
		if matches {
			handles = append(handles, fh)
		}
	}
	fs.mu.RUnlock()
	for _, fh := range handles {
		fh.mu.Lock()
		if fh.cacheFile == nil || fh.cacheGeneration != -1 {
			f, openErr := os.OpenFile(ov.BackingPath, os.O_RDWR, 0)
			if openErr != nil {
				f, openErr = os.Open(ov.BackingPath)
				if openErr != nil {
					fh.mu.Unlock()
					return openErr
				}
			}
			old := fh.cacheFile
			fh.cacheFile = f
			fh.cacheGeneration = -1
			if old != nil {
				_ = old.Close()
			}
		}
		fh.mu.Unlock()
	}
	return nil
}

func (fs *ArtifactFuse) detachOpenHandles(path string) {
	fs.mu.RLock()
	var handles []*FileHandle
	for _, fh := range fs.fileHandles {
		fh.mu.Lock()
		matches := samePathOrDescendant(fh.path, path)
		fh.mu.Unlock()
		if matches {
			handles = append(handles, fh)
		}
	}
	fs.mu.RUnlock()
	for _, fh := range handles {
		fh.mu.Lock()
		fh.detached = true
		fh.mu.Unlock()
	}
}

func (fs *ArtifactFuse) moveOpenHandles(oldPath, newPath string) {
	fs.mu.RLock()
	var handles []*FileHandle
	for _, fh := range fs.fileHandles {
		fh.mu.Lock()
		matches := samePathOrDescendant(fh.path, oldPath)
		fh.mu.Unlock()
		if matches {
			handles = append(handles, fh)
		}
	}
	fs.mu.RUnlock()
	for _, fh := range handles {
		fh.mu.Lock()
		fh.path = newPath + strings.TrimPrefix(fh.path, oldPath)
		fh.mu.Unlock()
	}
}

func (fh *FileHandle) read(ctx context.Context, engine *Engine, off int64, size int) ([]byte, error) {
	fh.mu.Lock()
	path := fh.path
	if fh.detached && fh.cacheFile != nil {
		defer fh.mu.Unlock()
		return readFileChunkFrom(fh.cacheFile, off, size)
	}
	fh.mu.Unlock()
	currentGen := engine.Resolver.Generation()
	fh.mu.Lock()
	if fh.cacheFile != nil && (fh.cacheGeneration == -1 || fh.cacheGeneration == currentGen) {
		defer fh.mu.Unlock()
		return readFileChunkFrom(fh.cacheFile, off, size)
	}
	if fh.cacheFile != nil {
		f := fh.cacheFile
		fh.cacheFile = nil
		fh.cacheGeneration = 0
		fh.invalidateSeq++
		fh.mu.Unlock()
		_ = f.Close()
		fh.mu.Lock()
	}
	seq := fh.invalidateSeq
	fh.mu.Unlock()

	f, gen, ok, err := engine.BaseCacheFile(ctx, path)
	if err != nil {
		return nil, err
	}
	if !ok {
		return engine.Read(ctx, path, off, size)
	}
	fh.mu.Lock()
	if fh.invalidateSeq != seq || gen != engine.Resolver.Generation() {
		fh.mu.Unlock()
		_ = f.Close()
		return engine.Read(ctx, path, off, size)
	}
	if fh.cacheFile != nil && fh.cacheGeneration == gen {
		_ = f.Close()
		f = fh.cacheFile
	} else {
		if fh.cacheFile != nil {
			_ = fh.cacheFile.Close()
		}
		fh.cacheFile = f
		fh.cacheGeneration = gen
	}
	defer fh.mu.Unlock()
	return readFileChunkFrom(f, off, size)
}

func (fh *FileHandle) closeCachedFile() {
	fh.mu.Lock()
	if fh.detached {
		fh.mu.Unlock()
		return
	}
	f := fh.cacheFile
	fh.cacheFile = nil
	fh.cacheGeneration = 0
	fh.invalidateSeq++
	fh.mu.Unlock()
	if f != nil {
		_ = f.Close()
	}
}

func (fh *FileHandle) release() {
	fh.mu.Lock()
	f := fh.cacheFile
	fh.cacheFile = nil
	fh.mu.Unlock()
	if f != nil {
		_ = f.Close()
	}
}

// --- FUSE operations ---

func (fs *ArtifactFuse) StatFS(_ context.Context, op *fuseops.StatFSOp) error {
	const blockSize = 4096
	const totalSpace = 1 * 1024 * 1024 * 1024 * 1024 * 1024
	const totalBlocks = totalSpace / blockSize
	op.BlockSize = blockSize
	op.Blocks = totalBlocks
	op.BlocksFree = totalBlocks
	op.BlocksAvailable = totalBlocks
	op.IoSize = 1 * 1024 * 1024
	op.Inodes = 1_000_000_000
	op.InodesFree = 1_000_000_000
	return nil
}

func (fs *ArtifactFuse) LookUpInode(ctx context.Context, op *fuseops.LookUpInodeOp) error {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	parent, err := fs.requireInode(op.Parent, syscall.ENOENT)
	if err != nil {
		return err
	}
	if parent.Detached {
		return syscall.ENOENT
	}

	childPath := cleanChildPath(parent.Path, op.Name)

	// Synthesize .git gitfile in root
	if parent.IsRoot && op.Name == ".git" {
		fs.mu.Lock()
		ref := fs.allocInode(".git", "file", 0o644, fs.resolver.Generation())
		fs.mu.Unlock()
		op.Entry.Child = ref.ID
		op.Entry.Attributes = fs.gitFileAttrs()
		setChildEntryExpiry(&op.Entry, time.Minute)
		return nil
	}

	mode, size, typ, mtime, ctime, err := fs.resolveAttrs(ctx, childPath)
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return syscall.ENOENT
		}
		return syscall.EIO
	}

	fs.mu.Lock()
	ref := fs.allocInode(childPath, typ, mode, fs.resolver.Generation())
	fs.mu.Unlock()

	op.Entry.Child = ref.ID
	op.Entry.Attributes = inodeAttrs(mode, uint64(size), typ, mtime, ctime)
	setChildEntryExpiry(&op.Entry, time.Second)
	return nil
}

func (fs *ArtifactFuse) GetInodeAttributes(ctx context.Context, op *fuseops.GetInodeAttributesOp) error {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	ref, err := fs.requireInode(op.Inode, syscall.ESTALE)
	if err != nil {
		return err
	}
	if ref.Detached {
		now := time.Now()
		op.Attributes = inodeAttrs(ref.Mode, 4096, ref.Type, now, now)
		op.AttributesExpiration = attrExpiry(time.Second)
		return nil
	}

	if ref.IsRoot {
		if fs.resolver != nil {
			if mode, size, typ, mtime, ctime, err := fs.resolver.Getattr(ref.Path); err == nil {
				op.Attributes = inodeAttrs(mode, uint64(size), typ, mtime, ctime)
				op.AttributesExpiration = attrExpiry(time.Second)
				return nil
			}
		}
		now := time.Now()
		op.Attributes = inodeAttrs(ref.Mode, 4096, "dir", now, now)
		op.AttributesExpiration = attrExpiry(time.Second)
		return nil
	}

	if ref.Path == ".git" {
		op.Attributes = fs.gitFileAttrs()
		op.AttributesExpiration = attrExpiry(time.Minute)
		return nil
	}

	mode, size, typ, mtime, ctime, err := fs.resolveAttrs(ctx, ref.Path)
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return syscall.ENOENT
		}
		return syscall.EIO
	}
	op.Attributes = inodeAttrs(mode, uint64(size), typ, mtime, ctime)
	op.AttributesExpiration = attrExpiry(time.Second)
	return nil
}

func (fs *ArtifactFuse) resolveAttrs(ctx context.Context, path string) (mode uint32, size int64, nodeType string, mtime time.Time, ctime time.Time, err error) {
	n, generation, commitTime, err := fs.resolver.ResolvePathState(path)
	if err != nil {
		return 0, 0, "", time.Time{}, time.Time{}, err
	}
	if n.FromOverlay {
		typ := n.Overlay.NodeType()
		mt := time.Unix(0, n.Overlay.MtimeUnixNs)
		ct := time.Unix(0, n.Overlay.CtimeUnixNs)
		return n.Overlay.Mode, n.Overlay.SizeBytes, typ, mt, ct, nil
	}

	mode = normalizeMode(n.Base.Mode, n.Base.Type)
	size = n.Base.SizeBytes
	if n.Base.Type == "file" && n.Base.SizeState != "known" && n.Base.ObjectOID != "" {
		_, hydratedSize, hErr := fs.engine.Hydrator.EnsureHydrated(ctx, fs.repo, n.Base)
		if hErr != nil {
			return 0, 0, "", time.Time{}, time.Time{}, hErr
		}
		size = hydratedSize
	} else if n.Base.Type == "symlink" && n.Base.SizeState != "known" && n.Base.ObjectOID != "" {
		target, readErr := fs.engine.Hydrator.ReadBlob(ctx, fs.repo, n.Base, model.MaxSymlinkTargetBytes)
		if readErr != nil {
			return 0, 0, "", time.Time{}, time.Time{}, readErr
		}
		size = int64(len(target))
	}

	// Base files use the HEAD commit timestamp for mtime so tools like
	// make see a stable, meaningful value.
	ct := commitTime
	if ct == 0 {
		ct = generation // fallback: commit time unavailable
	}
	mt := time.Unix(ct, 0)
	return mode, size, n.Base.Type, mt, mt, nil
}

func (fs *ArtifactFuse) SetInodeAttributes(ctx context.Context, op *fuseops.SetInodeAttributesOp) error {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	ref, err := fs.requireInode(op.Inode, syscall.ESTALE)
	if err != nil {
		return err
	}
	if ref.Detached {
		return syscall.ESTALE
	}
	if op.Size != nil {
		fs.closeCachedFilesForPath(ref.Path)
		if err := fs.engine.Truncate(ctx, ref.Path, int64(*op.Size)); err != nil {
			return syscall.EIO
		}
		fs.closeCachedFilesForPath(ref.Path)
	}
	if op.Mode != nil {
		if err := fs.engine.SetMode(ctx, ref.Path, uint32(op.Mode.Perm())); err != nil {
			if errors.Is(err, iofs.ErrInvalid) {
				return syscall.ENOTSUP
			}
			if errors.Is(err, iofs.ErrNotExist) {
				return syscall.ENOENT
			}
			return syscall.EIO
		}
	}
	// Handle mtime updates (e.g., from touch)
	if op.Mtime != nil {
		fs.closeCachedFilesForPath(ref.Path)
		if err := fs.engine.SetMtime(ctx, ref.Path, *op.Mtime); err != nil {
			if errors.Is(err, iofs.ErrInvalid) {
				return syscall.ENOTSUP
			}
			return syscall.EIO
		}
		fs.closeCachedFilesForPath(ref.Path)
	}
	mode, size, typ, mtime, ctime, err := fs.resolver.Getattr(ref.Path)
	if err != nil {
		return syscall.EIO
	}
	op.Attributes = inodeAttrs(mode, uint64(size), typ, mtime, ctime)
	op.AttributesExpiration = attrExpiry(time.Second)
	return nil
}

func (fs *ArtifactFuse) ForgetInode(_ context.Context, op *fuseops.ForgetInodeOp) error {
	fs.mu.Lock()
	ref, ok := fs.inodes[op.Inode]
	if ok {
		ref.Refcnt -= int64(op.N)
		if ref.Refcnt <= 0 && !ref.IsRoot {
			delete(fs.inodes, op.Inode)
			if fs.pathToInode[ref.Path] == op.Inode {
				delete(fs.pathToInode, ref.Path)
			}
		}
	}
	fs.mu.Unlock()
	return nil
}

func (fs *ArtifactFuse) OpenDir(ctx context.Context, op *fuseops.OpenDirOp) error {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	ref, err := fs.requireInode(op.Inode, syscall.ESTALE)
	if err != nil {
		return err
	}
	if ref.Detached {
		fs.mu.Lock()
		handle := fs.nextHandleID
		fs.nextHandleID++
		fs.dirHandles[handle] = &DirHandle{inode: ref, gen: ref.Gen}
		fs.mu.Unlock()
		op.Handle = handle
		return nil
	}
	// Eagerly load children at open time to avoid races on concurrent ReadDir.
	entries, gen, commitTime, err := fs.resolver.ReaddirSnapshot(ctx, ref.Path)
	if err != nil {
		return syscall.EIO
	}
	if ref.IsRoot {
		entries = append([]ReaddirEntry{{Name: ".git", Type: "file", Mode: 0o644, SizeBytes: int64(len(fs.gitfileContent)), SizeState: "known"}}, entries...)
	}

	dh := &DirHandle{inode: ref, gen: gen, commitTime: commitTime, entries: entries}
	fs.mu.Lock()
	handle := fs.nextHandleID
	fs.nextHandleID++
	fs.dirHandles[handle] = dh
	fs.mu.Unlock()
	op.Handle = handle
	return nil
}

func (fs *ArtifactFuse) ReadDir(_ context.Context, op *fuseops.ReadDirOp) error {
	dh, err := fs.dirHandle(op.Handle)
	if err != nil {
		return err
	}

	offset := int(op.Offset)
	for i := offset; i < len(dh.entries); i++ {
		e := dh.entries[i]
		dirent := fuseutil.Dirent{
			Offset: fuseops.DirOffset(i + 1),
			Inode:  fuseops.RootInodeID + 1, // placeholder; kernel re-looks-up via LookUpInode
			Name:   e.Name,
			Type:   e.direntType(),
		}
		n := fuseutil.WriteDirent(op.Dst[op.BytesRead:], dirent)
		if n == 0 {
			break
		}
		op.BytesRead += n
	}
	return nil
}

func (fs *ArtifactFuse) ReadDirPlus(_ context.Context, op *fuseops.ReadDirPlusOp) error {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	dh, err := fs.dirHandle(op.Handle)
	if err != nil {
		return err
	}

	offset := int(op.Offset)
	for i := offset; i < len(dh.entries); i++ {
		e := dh.entries[i]
		childPath := cleanChildPath(dh.inode.Path, e.Name)
		entry, err := fs.childEntryFromReaddir(childPath, dh.gen, dh.commitTime, e)
		if err != nil {
			if errors.Is(err, iofs.ErrNotExist) {
				return syscall.ENOENT
			}
			return syscall.EIO
		}
		dirent := fuseutil.DirentPlus{
			Dirent: fuseutil.Dirent{
				Offset: fuseops.DirOffset(i + 1),
				Inode:  entry.Child,
				Name:   e.Name,
				Type:   e.direntType(),
			},
			Entry: entry,
		}
		n := fuseutil.WriteDirentPlus(op.Dst[op.BytesRead:], dirent)
		if n == 0 {
			fs.dropInodeLookup(entry.Child)
			break
		}
		op.BytesRead += n
	}
	return nil
}

func (fs *ArtifactFuse) childEntryFromReaddir(path string, gen, commitTime int64, e ReaddirEntry) (fuseops.ChildInodeEntry, error) {
	if path == ".git" {
		fs.mu.Lock()
		ref := fs.allocInode(path, "file", 0o644, gen)
		fs.mu.Unlock()
		entry := fuseops.ChildInodeEntry{Child: ref.ID, Attributes: fs.gitFileAttrs()}
		setChildEntryExpiry(&entry, time.Minute)
		return entry, nil
	}
	mode, size, typ, mtime, ctime := readdirAttrs(e, gen, commitTime)
	fs.mu.Lock()
	ref := fs.allocInode(path, typ, mode, gen)
	fs.mu.Unlock()
	entry := fuseops.ChildInodeEntry{
		Child:      ref.ID,
		Attributes: inodeAttrs(mode, uint64(size), typ, mtime, ctime),
	}
	setChildEntryExpiry(&entry, time.Second)
	return entry, nil
}

func readdirAttrs(e ReaddirEntry, gen, commitTime int64) (mode uint32, size int64, typ string, mtime time.Time, ctime time.Time) {
	typ = e.Type
	mode = normalizeMode(e.Mode, typ)
	size = e.SizeBytes
	if e.FromOverlay {
		return mode, size, typ, time.Unix(0, e.MtimeUnixNs), time.Unix(0, e.CtimeUnixNs)
	}
	ct := commitTime
	if ct == 0 {
		ct = gen
	}
	mt := time.Unix(ct, 0)
	return mode, size, typ, mt, mt
}

func (fs *ArtifactFuse) ReleaseDirHandle(_ context.Context, op *fuseops.ReleaseDirHandleOp) error {
	fs.mu.Lock()
	delete(fs.dirHandles, op.Handle)
	fs.mu.Unlock()
	return nil
}

func (fs *ArtifactFuse) OpenFile(ctx context.Context, op *fuseops.OpenFileOp) error {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	ref, err := fs.requireInode(op.Inode, syscall.ESTALE)
	if err != nil {
		return err
	}
	fh := &FileHandle{inode: ref, path: ref.Path}
	if ref.Path != ".git" {
		if ov, ok, err := fs.engine.Overlay.Lookup(ctx, ref.Path); err != nil {
			return syscall.EIO
		} else if ok && !ov.IsDeleted() {
			f, err := os.OpenFile(ov.BackingPath, os.O_RDWR, 0)
			if err != nil {
				f, err = os.Open(ov.BackingPath)
			}
			if err != nil {
				return syscall.EIO
			}
			fh.cacheFile = f
			fh.cacheGeneration = -1
		} else {
			f, gen, base, err := fs.engine.BaseCacheFile(ctx, ref.Path)
			if err != nil {
				return syscall.EIO
			}
			if base {
				fh.cacheFile = f
				fh.cacheGeneration = gen
			}
		}
	}
	fs.mu.Lock()
	handle := fs.nextHandleID
	fs.nextHandleID++
	fs.fileHandles[handle] = fh
	fs.mu.Unlock()
	op.Handle = handle
	op.KeepPageCache = false
	return nil
}

func (fs *ArtifactFuse) ReadFile(ctx context.Context, op *fuseops.ReadFileOp) error {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	fh, err := fs.fileHandle(op.Handle)
	if err != nil {
		return err
	}

	fh.mu.Lock()
	path := fh.path
	fh.mu.Unlock()
	if path == ".git" {
		start := int(op.Offset)
		if start >= len(fs.gitfileContent) {
			op.BytesRead = 0
			return nil
		}
		end := min(start+int(op.Size), len(fs.gitfileContent))
		op.Data = [][]byte{fs.gitfileContent[start:end]}
		op.BytesRead = end - start
		return nil
	}

	data, err := fh.read(ctx, fs.engine, op.Offset, int(op.Size))
	if err != nil {
		if os.IsNotExist(err) {
			return syscall.ENOENT
		}
		return syscall.EIO
	}
	op.Data = [][]byte{data}
	op.BytesRead = len(data)
	return nil
}

func (fs *ArtifactFuse) WriteFile(ctx context.Context, op *fuseops.WriteFileOp) error {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	fh, err := fs.fileHandle(op.Handle)
	if err != nil {
		return err
	}
	fh.mu.Lock()
	if fh.detached {
		f := fh.cacheFile
		fh.mu.Unlock()
		if f == nil {
			return syscall.EIO
		}
		if _, err := f.WriteAt(op.Data, op.Offset); err != nil {
			return syscall.EIO
		}
		return nil
	}
	path := fh.path
	fh.mu.Unlock()
	fs.closeCachedFilesForPath(path)
	n, err := fs.engine.Write(ctx, path, op.Offset, op.Data)
	if err != nil {
		return syscall.EIO
	}
	if n != len(op.Data) {
		return syscall.EIO
	}
	fs.closeCachedFilesForPath(path)
	return nil
}

func (fs *ArtifactFuse) CreateFile(ctx context.Context, op *fuseops.CreateFileOp) error {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	_, childPath, err := fs.childPath(op.Parent, op.Name)
	if err != nil {
		return err
	}
	if err := fs.engine.Create(ctx, childPath, uint32(op.Mode)); err != nil {
		return syscall.EIO
	}
	fs.mu.Lock()
	ref := fs.allocInode(childPath, "file", uint32(op.Mode), fs.resolver.Generation())
	fh := &FileHandle{inode: ref, path: childPath}
	handle := fs.nextHandleID
	fs.nextHandleID++
	fs.fileHandles[handle] = fh
	fs.mu.Unlock()

	op.Entry.Child = ref.ID
	now := time.Now()
	op.Entry.Attributes = inodeAttrs(uint32(op.Mode), 0, "file", now, now)
	setChildEntryExpiry(&op.Entry, time.Second)
	op.Handle = handle
	return nil
}

func (fs *ArtifactFuse) CreateSymlink(ctx context.Context, op *fuseops.CreateSymlinkOp) error {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	_, childPath, err := fs.childPath(op.Parent, op.Name)
	if err != nil {
		return err
	}
	if len(op.Target) > model.MaxSymlinkTargetBytes {
		return syscall.ENAMETOOLONG
	}
	if err := fs.engine.Symlink(ctx, childPath, op.Target); err != nil {
		return syscall.EIO
	}
	fs.mu.Lock()
	ref := fs.allocInode(childPath, "symlink", 0o120000, fs.resolver.Generation())
	fs.mu.Unlock()

	op.Entry.Child = ref.ID
	now := time.Now()
	op.Entry.Attributes = inodeAttrs(0o120000, uint64(len(op.Target)), "symlink", now, now)
	setChildEntryExpiry(&op.Entry, time.Second)
	return nil
}

func (fs *ArtifactFuse) MkDir(ctx context.Context, op *fuseops.MkDirOp) error {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	_, childPath, err := fs.childPath(op.Parent, op.Name)
	if err != nil {
		return err
	}
	if err := fs.engine.Mkdir(ctx, childPath, uint32(op.Mode)); err != nil {
		return syscall.EIO
	}
	fs.mu.Lock()
	ref := fs.allocInode(childPath, "dir", uint32(op.Mode), fs.resolver.Generation())
	fs.mu.Unlock()

	op.Entry.Child = ref.ID
	now := time.Now()
	op.Entry.Attributes = inodeAttrs(uint32(op.Mode)|uint32(os.ModeDir), 4096, "dir", now, now)
	setChildEntryExpiry(&op.Entry, time.Second)
	return nil
}

func (fs *ArtifactFuse) RmDir(ctx context.Context, op *fuseops.RmDirOp) error {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	_, childPath, err := fs.childPath(op.Parent, op.Name)
	if err != nil {
		return err
	}
	if err := fs.engine.Rmdir(ctx, childPath); err != nil {
		if os.IsExist(err) {
			return syscall.ENOTEMPTY
		}
		return syscall.EIO
	}
	return nil
}

func (fs *ArtifactFuse) Unlink(ctx context.Context, op *fuseops.UnlinkOp) error {
	fs.handleOps.Lock()
	defer fs.handleOps.Unlock()
	_, childPath, err := fs.childPath(op.Parent, op.Name)
	if err != nil {
		return err
	}
	if err := fs.engine.ensureOverlay(ctx, childPath); err != nil {
		return syscall.EIO
	}
	if err := fs.pinOpenHandles(childPath); err != nil {
		return syscall.EIO
	}
	if err := fs.engine.Unlink(ctx, childPath); err != nil {
		return syscall.EIO
	}
	fs.detachOpenHandles(childPath)
	return nil
}

func (fs *ArtifactFuse) Rename(ctx context.Context, op *fuseops.RenameOp) error {
	fs.handleOps.Lock()
	defer fs.handleOps.Unlock()
	oldParent, err := fs.requireInode(op.OldParent, syscall.ENOENT)
	if err != nil {
		return err
	}
	newParent, err := fs.requireInode(op.NewParent, syscall.ENOENT)
	if err != nil {
		return err
	}
	if oldParent.Detached || newParent.Detached {
		return syscall.ENOENT
	}
	oldPath := cleanChildPath(oldParent.Path, op.OldName)
	newPath := cleanChildPath(newParent.Path, op.NewName)
	source, err := fs.resolver.ResolvePath(oldPath)
	if err != nil {
		return syscall.ENOENT
	}
	sourceType := resolvedNodeType(source)
	if oldPath == newPath {
		return nil
	}
	if sourceType == "dir" && strings.HasPrefix(newPath, oldPath+"/") {
		return syscall.EINVAL
	}
	if destination, err := fs.resolver.ResolvePath(newPath); err == nil {
		destinationType := resolvedNodeType(destination)
		if sourceType == "dir" && destinationType != "dir" {
			return syscall.ENOTDIR
		}
		if sourceType != "dir" && destinationType == "dir" {
			return syscall.EISDIR
		}
	}
	if sourceType != "dir" {
		if err := fs.engine.ensureOverlay(ctx, oldPath); err != nil {
			return syscall.EIO
		}
		if err := fs.pinOpenHandles(oldPath); err != nil {
			return syscall.EIO
		}
		if _, err := fs.resolver.ResolvePath(newPath); err == nil {
			if err := fs.engine.ensureOverlay(ctx, newPath); err != nil {
				return syscall.EIO
			}
			if err := fs.pinOpenHandles(newPath); err != nil {
				return syscall.EIO
			}
		}
	}
	if err := fs.engine.Rename(ctx, oldPath, newPath); err != nil {
		if errors.Is(err, iofs.ErrInvalid) {
			return syscall.EINVAL
		}
		if os.IsExist(err) {
			return syscall.ENOTEMPTY
		}
		if errors.Is(err, iofs.ErrNotExist) {
			return syscall.ENOENT
		}
		return syscall.EIO
	}
	fs.moveInodePath(oldPath, newPath)
	fs.detachOpenHandles(newPath)
	fs.moveOpenHandles(oldPath, newPath)
	return nil
}

func (fs *ArtifactFuse) ReadSymlink(ctx context.Context, op *fuseops.ReadSymlinkOp) error {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	ref, err := fs.requireInode(op.Inode, syscall.ESTALE)
	if err != nil {
		return err
	}
	n, err := fs.resolver.ResolvePath(ref.Path)
	if err != nil {
		return syscall.ENOENT
	}
	if n.FromOverlay && n.Overlay.Kind == model.OverlayKindSymlink {
		if len(n.Overlay.TargetPath) > model.MaxSymlinkTargetBytes {
			return syscall.ENAMETOOLONG
		}
		op.Target = n.Overlay.TargetPath
		return nil
	}
	if n.Base.ObjectOID != "" {
		if err := validateKnownSymlinkTargetSize(n.Base); err != nil {
			return err
		}
		data, err := fs.engine.Hydrator.ReadBlob(ctx, fs.repo, n.Base, model.MaxSymlinkTargetBytes)
		if err != nil {
			if errors.Is(err, model.ErrBlobTooLarge) {
				return syscall.ENAMETOOLONG
			}
			return syscall.EIO
		}
		op.Target = string(data)
		return nil
	}
	return syscall.ENOENT
}

func validateKnownSymlinkTargetSize(node model.BaseNode) error {
	if node.SizeState != "known" {
		return nil
	}
	if node.SizeBytes < 0 {
		return syscall.EIO
	}
	if node.SizeBytes > model.MaxSymlinkTargetBytes {
		return syscall.ENAMETOOLONG
	}
	return nil
}

func (fs *ArtifactFuse) FlushFile(_ context.Context, _ *fuseops.FlushFileOp) error {
	return nil
}

func (fs *ArtifactFuse) SyncFile(ctx context.Context, op *fuseops.SyncFileOp) error {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	fh, err := fs.fileHandle(op.Handle)
	if err != nil {
		return err
	}
	fh.mu.Lock()
	if fh.detached {
		f := fh.cacheFile
		fh.mu.Unlock()
		if f == nil || f.Sync() != nil {
			return syscall.EIO
		}
		return nil
	}
	path := fh.path
	if fh.cacheFile != nil && fh.cacheGeneration >= 0 {
		fh.mu.Unlock()
		return nil
	}
	fh.mu.Unlock()
	if err := fs.engine.Sync(ctx, path); err != nil {
		return syscall.EIO
	}
	return nil
}

func (fs *ArtifactFuse) ReleaseFileHandle(_ context.Context, op *fuseops.ReleaseFileHandleOp) error {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	fs.mu.Lock()
	fh := fs.fileHandles[op.Handle]
	delete(fs.fileHandles, op.Handle)
	fs.mu.Unlock()
	if fh != nil {
		fh.release()
	}
	return nil
}

func (fs *ArtifactFuse) GetXattr(_ context.Context, _ *fuseops.GetXattrOp) error {
	return syscall.ENOSYS
}
func (fs *ArtifactFuse) ListXattr(_ context.Context, _ *fuseops.ListXattrOp) error {
	return syscall.ENOSYS
}
func (fs *ArtifactFuse) SetXattr(_ context.Context, _ *fuseops.SetXattrOp) error {
	return syscall.ENOSYS
}
func (fs *ArtifactFuse) RemoveXattr(_ context.Context, _ *fuseops.RemoveXattrOp) error {
	return syscall.ENOSYS
}

// --- Mount lifecycle ---

type mountedFSWrapper struct {
	*fuse.MountedFileSystem
	mountPoint string
}

func (m *mountedFSWrapper) Unmount() error {
	return TryUnmount(m.mountPoint)
}

func MountRepo(repo model.RepoConfig, resolver *Resolver, engine *Engine) (MountedFS, error) {
	return MountRepoWithGate(repo, resolver, engine, nil)
}

func MountRepoWithGate(repo model.RepoConfig, resolver *Resolver, engine *Engine, gate *ReadyGate) (MountedFS, error) {
	fsint := NewArtifactFuse(repo, resolver, engine)
	server := fuseutil.NewFileSystemServer(NewGatedFileSystem(fsint, gate))

	mountCfg := &fuse.MountConfig{
		FSName:                  "artifact-fs:" + repo.Name,
		Subtype:                 "artifact-fs",
		DisableWritebackCaching: true,
		UseVectoredRead:         true,
	}
	// READDIRPLUS would cache unknown blob sizes as zero before lookup can hydrate them.
	platformMountConfig(mountCfg)

	mfs, err := fuse.Mount(repo.MountPath, server, mountCfg)
	if err != nil {
		return nil, fmt.Errorf("fuse mount %s: %w", repo.MountPath, err)
	}

	return &mountedFSWrapper{MountedFileSystem: mfs, mountPoint: repo.MountPath}, nil
}

func TryUnmount(mountPoint string) error {
	var err error
	for range 20 {
		err = fuse.Unmount(mountPoint)
		if err == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	return err
}

func inodeAttrs(mode uint32, size uint64, typ string, mtime time.Time, ctime time.Time) fuseops.InodeAttributes {
	m := os.FileMode(mode & 0o777)
	switch typ {
	case "dir":
		m |= os.ModeDir
		if size == 0 {
			size = 4096
		}
	case "symlink":
		m |= os.ModeSymlink
	}
	return fuseops.InodeAttributes{
		Size:  size,
		Nlink: 1,
		Mode:  m,
		Uid:   uint32(os.Getuid()),
		Gid:   uint32(os.Getgid()),
		Atime: mtime,
		Mtime: mtime,
		Ctime: ctime,
	}
}

func cleanChildPath(parentPath string, name string) string {
	return model.CleanPath(filepath.Join(parentPath, name))
}

func attrExpiry(ttl time.Duration) time.Time {
	return time.Now().Add(ttl)
}

func setChildEntryExpiry(entry *fuseops.ChildInodeEntry, ttl time.Duration) {
	expiresAt := attrExpiry(ttl)
	entry.AttributesExpiration = expiresAt
	entry.EntryExpiration = expiresAt
}

func (fs *ArtifactFuse) gitFileAttrs() fuseops.InodeAttributes {
	now := time.Now()
	return fuseops.InodeAttributes{
		Size:  uint64(len(fs.gitfileContent)),
		Mode:  0o644,
		Nlink: 1,
		Uid:   uint32(os.Getuid()),
		Gid:   uint32(os.Getgid()),
		Atime: now,
		Mtime: now,
		Ctime: now,
	}
}
