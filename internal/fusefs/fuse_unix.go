//go:build !windows

package fusefs

import (
	"context"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
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

	handleMu     sync.Mutex
	mu           sync.RWMutex
	inodes       map[fuseops.InodeID]*InodeRef
	pathToInode  map[string]fuseops.InodeID
	nextInodeID  fuseops.InodeID
	dirHandles   map[fuseops.HandleID]*DirHandle
	fileHandles  map[fuseops.HandleID]*FileHandle
	filesByPath  map[string]map[fuseops.HandleID]*FileHandle
	nextHandleID fuseops.HandleID
}

type InodeRef struct {
	ID      fuseops.InodeID
	Path    string
	Type    string // file, dir, symlink
	Mode    uint32
	Gen     int64
	Refcnt  int64
	IsRoot  bool
	Overlay bool
}

type DirHandle struct {
	inode      *InodeRef
	gen        int64
	commitTime int64
	entries    []ReaddirEntry
}

type FileHandle struct {
	mu              sync.RWMutex
	inode           *InodeRef
	path            string
	cacheFile       *os.File
	cacheGeneration int64
	invalidateSeq   uint64
	upperFile       *os.File
	upperWritable   bool
	detached        bool
	writable        bool
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
		filesByPath:    make(map[string]map[fuseops.HandleID]*FileHandle),
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

func (fs *ArtifactFuse) getInode(id fuseops.InodeID) *InodeRef {
	fs.mu.RLock()
	ref := fs.inodes[id]
	if ref != nil {
		copy := *ref
		ref = &copy
	}
	fs.mu.RUnlock()
	return ref
}

func (fs *ArtifactFuse) requireInode(id fuseops.InodeID, missing error) (*InodeRef, error) {
	ref := fs.getInode(id)
	if ref == nil {
		return nil, missing
	}
	return ref, nil
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

func (fs *ArtifactFuse) childPath(parentID fuseops.InodeID, name string) (*InodeRef, string, error) {
	parent, err := fs.requireInode(parentID, syscall.ENOENT)
	if err != nil {
		return nil, "", err
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
	byID := fs.filesByPath[path]
	handles := make([]*FileHandle, 0, len(byID))
	for _, fh := range byID {
		handles = append(handles, fh)
	}
	fs.mu.RUnlock()
	for _, fh := range handles {
		fh.closeCachedFile()
	}
}

func (fs *ArtifactFuse) lockFileHandlesForPaths(paths ...string) func() {
	fs.mu.RLock()
	byID := make(map[fuseops.HandleID]*FileHandle)
	for _, path := range paths {
		for id, fh := range fs.filesByPath[path] {
			byID[id] = fh
		}
	}
	fs.mu.RUnlock()
	ids := make([]fuseops.HandleID, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		byID[id].mu.Lock()
	}
	return func() {
		for i := len(ids) - 1; i >= 0; i-- {
			byID[ids[i]].mu.Unlock()
		}
	}
}

func (fs *ArtifactFuse) fileHandlesForPath(path string) map[fuseops.HandleID]*FileHandle {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	handles := make(map[fuseops.HandleID]*FileHandle, len(fs.filesByPath[path]))
	for id, fh := range fs.filesByPath[path] {
		handles[id] = fh
	}
	return handles
}

func (fh *FileHandle) read(ctx context.Context, engine *Engine, off int64, size int) ([]byte, error) {
	currentGen := engine.Resolver.Generation()
	fh.mu.RLock()
	path := fh.path
	if fh.upperFile != nil {
		defer fh.mu.RUnlock()
		return readFileChunkFrom(fh.upperFile, off, size)
	}
	if fh.cacheFile != nil && fh.cacheGeneration == currentGen {
		defer fh.mu.RUnlock()
		return readFileChunkFrom(fh.cacheFile, off, size)
	}
	fh.mu.RUnlock()

	fh.mu.Lock()
	if fh.cacheFile != nil && fh.cacheGeneration == currentGen {
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

	cachePath, gen, ok, err := engine.BaseCachePath(ctx, path)
	if err != nil {
		return nil, err
	}
	if !ok {
		return engine.Read(ctx, path, off, size)
	}
	f, err := os.Open(cachePath)
	if err != nil {
		return nil, err
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

func (fh *FileHandle) pathSnapshot() string {
	fh.mu.RLock()
	defer fh.mu.RUnlock()
	return fh.path
}

func (fh *FileHandle) pinUpper(path string) error {
	fh.mu.RLock()
	if fh.upperFile != nil {
		fh.mu.RUnlock()
		return nil
	}
	fh.mu.RUnlock()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	writable := err == nil
	if err != nil {
		f, err = os.Open(path)
		if err != nil {
			return err
		}
	}
	fh.mu.Lock()
	if fh.upperFile == nil {
		fh.upperFile = f
		fh.upperWritable = writable
		f = nil
	}
	fh.mu.Unlock()
	if f != nil {
		return f.Close()
	}
	return nil
}

func (fh *FileHandle) pinReadOnly(path string) error {
	fh.mu.RLock()
	if fh.upperFile != nil {
		fh.mu.RUnlock()
		return nil
	}
	fh.mu.RUnlock()
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	fh.mu.Lock()
	if fh.upperFile == nil {
		fh.upperFile = f
		f = nil
	}
	fh.mu.Unlock()
	if f != nil {
		return f.Close()
	}
	return nil
}

func (fh *FileHandle) repinUpper(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	writable := err == nil
	if err != nil {
		f, err = os.Open(path)
		if err != nil {
			return err
		}
	}
	fh.mu.Lock()
	previous := fh.upperFile
	fh.upperFile = f
	fh.upperWritable = writable
	fh.mu.Unlock()
	if previous != nil {
		return previous.Close()
	}
	return nil
}

func (fh *FileHandle) pinCurrent(ctx context.Context, engine *Engine) error {
	path := fh.pathSnapshot()
	if ov, ok := engine.Overlay.Get(path); ok && !ov.IsDeleted() && ov.BackingPath != "" {
		return fh.pinUpper(ov.BackingPath)
	}
	cachePath, _, ok, err := engine.BaseCachePath(ctx, path)
	if err != nil {
		return err
	}
	if !ok {
		return os.ErrNotExist
	}
	return fh.pinReadOnly(cachePath)
}

func (fh *FileHandle) prepareDetach(repo model.RepoConfig) error {
	fh.mu.RLock()
	if !fh.writable || fh.upperWritable || fh.upperFile == nil {
		fh.mu.RUnlock()
		return nil
	}
	source := fh.upperFile
	info, err := source.Stat()
	if err != nil {
		fh.mu.RUnlock()
		return err
	}
	dir := repo.OverlayDir
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fh.mu.RUnlock()
		return err
	}
	private, err := os.CreateTemp(dir, ".artifact-fs-detached-*")
	if err != nil {
		fh.mu.RUnlock()
		return err
	}
	name := private.Name()
	_ = os.Remove(name)
	_, copyErr := io.Copy(private, io.NewSectionReader(source, 0, info.Size()))
	fh.mu.RUnlock()
	if copyErr != nil {
		_ = private.Close()
		return copyErr
	}
	fh.mu.Lock()
	if fh.upperFile == source && !fh.upperWritable {
		fh.upperFile = private
		fh.upperWritable = true
		private = nil
	}
	fh.mu.Unlock()
	if private != nil {
		return private.Close()
	}
	return source.Close()
}

func (fh *FileHandle) writeDetached(off int64, data []byte) (bool, error) {
	fh.mu.RLock()
	defer fh.mu.RUnlock()
	if !fh.detached {
		return false, nil
	}
	if !fh.writable || fh.upperFile == nil || !fh.upperWritable {
		return true, os.ErrPermission
	}
	_, err := fh.upperFile.WriteAt(data, off)
	return true, err
}

func (fh *FileHandle) syncPinned() (bool, error) {
	fh.mu.RLock()
	defer fh.mu.RUnlock()
	if fh.upperFile == nil {
		return false, nil
	}
	return true, fh.upperFile.Sync()
}

func (fh *FileHandle) closeCachedFile() {
	fh.mu.Lock()
	f := fh.cacheFile
	fh.cacheFile = nil
	fh.cacheGeneration = 0
	fh.invalidateSeq++
	fh.mu.Unlock()
	if f != nil {
		_ = f.Close()
	}
}

func (fh *FileHandle) closeFiles() {
	fh.mu.Lock()
	cacheFile := fh.cacheFile
	upperFile := fh.upperFile
	fh.cacheFile = nil
	fh.upperFile = nil
	fh.cacheGeneration = 0
	fh.invalidateSeq++
	fh.mu.Unlock()
	if cacheFile != nil {
		_ = cacheFile.Close()
	}
	if upperFile != nil {
		_ = upperFile.Close()
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
	parent, err := fs.requireInode(op.Parent, syscall.ENOENT)
	if err != nil {
		return err
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
	ref, err := fs.requireInode(op.Inode, syscall.ESTALE)
	if err != nil {
		return err
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
	n, err := fs.resolver.ResolvePath(path)
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
	}

	// Base files use the HEAD commit timestamp for mtime so tools like
	// make see a stable, meaningful value.
	ct := fs.resolver.CommitTime()
	if ct == 0 {
		ct = fs.resolver.Generation() // fallback: commit time unavailable
	}
	mt := time.Unix(ct, 0)
	return mode, size, n.Base.Type, mt, mt, nil
}

func (fs *ArtifactFuse) SetInodeAttributes(ctx context.Context, op *fuseops.SetInodeAttributesOp) error {
	ref, err := fs.requireInode(op.Inode, syscall.ESTALE)
	if err != nil {
		return err
	}
	if op.Size != nil {
		fs.closeCachedFilesForPath(ref.Path)
		if err := fs.engine.Truncate(ctx, ref.Path, int64(*op.Size)); err != nil {
			return syscall.EIO
		}
		fs.closeCachedFilesForPath(ref.Path)
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
	ref, err := fs.requireInode(op.Inode, syscall.ESTALE)
	if err != nil {
		return err
	}
	fs.resolver.viewMu.RLock()
	gen := fs.resolver.Generation()
	commitTime := fs.resolver.CommitTime()
	// Eagerly load children at open time to avoid races on concurrent ReadDir.
	entries, err := fs.resolver.readdirTypedAt(ctx, ref.Path, gen)
	fs.resolver.viewMu.RUnlock()
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
	dh, err := fs.dirHandle(op.Handle)
	if err != nil {
		return err
	}
	offset := int(op.Offset)
	for i := offset; i < len(dh.entries); i++ {
		e := dh.entries[i]
		if !e.FromOverlay && e.Type == "file" && e.SizeState != "known" {
			dirent := fuseutil.DirentPlus{
				Dirent: fuseutil.Dirent{Offset: fuseops.DirOffset(i + 1), Name: e.Name, Type: e.direntType()},
			}
			n := fuseutil.WriteDirentPlus(op.Dst[op.BytesRead:], dirent)
			if n == 0 {
				break
			}
			op.BytesRead += n
			continue
		}
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

func (fs *ArtifactFuse) OpenFile(_ context.Context, op *fuseops.OpenFileOp) error {
	fs.handleMu.Lock()
	defer fs.handleMu.Unlock()
	ref, err := fs.requireInode(op.Inode, syscall.ESTALE)
	if err != nil {
		return err
	}
	fh := &FileHandle{inode: ref, path: ref.Path, writable: !op.OpenFlags.IsReadOnly()}
	if ov, ok := fs.engine.Overlay.Get(ref.Path); ok && !ov.IsDeleted() && ov.BackingPath != "" {
		if err := fh.pinUpper(ov.BackingPath); err != nil {
			return syscall.EIO
		}
	}
	fs.mu.Lock()
	handle := fs.nextHandleID
	fs.nextHandleID++
	fs.fileHandles[handle] = fh
	if fs.filesByPath[fh.path] == nil {
		fs.filesByPath[fh.path] = make(map[fuseops.HandleID]*FileHandle)
	}
	fs.filesByPath[fh.path][handle] = fh
	fs.mu.Unlock()
	op.Handle = handle
	op.KeepPageCache = false
	return nil
}

func (fs *ArtifactFuse) ReadFile(ctx context.Context, op *fuseops.ReadFileOp) error {
	fh, err := fs.fileHandle(op.Handle)
	if err != nil {
		return err
	}

	path := fh.pathSnapshot()
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
	fs.handleMu.Lock()
	defer fs.handleMu.Unlock()
	fh, err := fs.fileHandle(op.Handle)
	if err != nil {
		return err
	}
	path := fh.pathSnapshot()
	if handled, err := fh.writeDetached(op.Offset, op.Data); handled {
		if err != nil {
			return syscall.EIO
		}
		return nil
	}
	fs.closeCachedFilesForPath(path)
	_, err = fs.engine.Write(ctx, path, op.Offset, op.Data)
	if err != nil {
		return syscall.EIO
	}
	if ov, ok := fs.engine.Overlay.Get(path); ok && ov.BackingPath != "" {
		if err := fh.repinUpper(ov.BackingPath); err != nil {
			return syscall.EIO
		}
	}
	fs.closeCachedFilesForPath(path)
	return nil
}

func (fs *ArtifactFuse) CreateFile(ctx context.Context, op *fuseops.CreateFileOp) error {
	fs.handleMu.Lock()
	defer fs.handleMu.Unlock()
	_, childPath, err := fs.childPath(op.Parent, op.Name)
	if err != nil {
		return err
	}
	if err := fs.engine.Create(ctx, childPath, uint32(op.Mode)); err != nil {
		if errors.Is(err, os.ErrExist) {
			return syscall.EEXIST
		}
		return syscall.EIO
	}
	fh := &FileHandle{path: childPath, writable: !op.OpenFlags.IsReadOnly()}
	if ov, ok := fs.engine.Overlay.Get(childPath); ok && ov.BackingPath != "" {
		if err := fh.pinUpper(ov.BackingPath); err != nil {
			return syscall.EIO
		}
	}
	fs.mu.Lock()
	ref := fs.allocInode(childPath, "file", uint32(op.Mode), fs.resolver.Generation())
	fh.inode = ref
	handle := fs.nextHandleID
	fs.nextHandleID++
	fs.fileHandles[handle] = fh
	if fs.filesByPath[childPath] == nil {
		fs.filesByPath[childPath] = make(map[fuseops.HandleID]*FileHandle)
	}
	fs.filesByPath[childPath][handle] = fh
	fs.mu.Unlock()

	op.Entry.Child = ref.ID
	now := time.Now()
	op.Entry.Attributes = inodeAttrs(uint32(op.Mode), 0, "file", now, now)
	setChildEntryExpiry(&op.Entry, time.Second)
	op.Handle = handle
	return nil
}

func (fs *ArtifactFuse) MkDir(ctx context.Context, op *fuseops.MkDirOp) error {
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
	fs.handleMu.Lock()
	defer fs.handleMu.Unlock()
	_, childPath, err := fs.childPath(op.Parent, op.Name)
	if err != nil {
		return err
	}
	handles := fs.fileHandlesForPath(childPath)
	for _, fh := range handles {
		if err := fh.pinCurrent(ctx, fs.engine); err != nil {
			return syscall.EIO
		}
		if err := fh.prepareDetach(fs.repo); err != nil {
			return syscall.EIO
		}
	}
	unlockHandles := fs.lockFileHandlesForPaths(childPath)
	if err := fs.engine.Unlink(ctx, childPath); err != nil {
		unlockHandles()
		return syscall.EIO
	}
	for _, fh := range handles {
		fh.detached = true
	}
	unlockHandles()
	return nil
}

func (fs *ArtifactFuse) Rename(ctx context.Context, op *fuseops.RenameOp) error {
	fs.handleMu.Lock()
	defer fs.handleMu.Unlock()
	oldParent, err := fs.requireInode(op.OldParent, syscall.ENOENT)
	if err != nil {
		return err
	}
	newParent, err := fs.requireInode(op.NewParent, syscall.ENOENT)
	if err != nil {
		return err
	}
	oldPath := cleanChildPath(oldParent.Path, op.OldName)
	newPath := cleanChildPath(newParent.Path, op.NewName)
	destinationHandles := fs.fileHandlesForPath(newPath)
	for _, fh := range destinationHandles {
		if err := fh.pinCurrent(ctx, fs.engine); err != nil {
			return syscall.EIO
		}
		if err := fh.prepareDetach(fs.repo); err != nil {
			return syscall.EIO
		}
	}
	fs.closeCachedFilesForPath(oldPath)
	unlockHandles := fs.lockFileHandlesForPaths(oldPath, newPath)
	if err := fs.engine.Rename(ctx, oldPath, newPath); err != nil {
		unlockHandles()
		if errors.Is(err, iofs.ErrInvalid) {
			return syscall.ENOTSUP
		}
		return syscall.EIO
	}
	fs.mu.Lock()
	if handles := fs.filesByPath[oldPath]; len(handles) > 0 {
		if fs.filesByPath[newPath] == nil {
			fs.filesByPath[newPath] = make(map[fuseops.HandleID]*FileHandle)
		}
		for id, fh := range handles {
			fh.path = newPath
			fs.filesByPath[newPath][id] = fh
			delete(destinationHandles, id)
		}
		delete(fs.filesByPath, oldPath)
	}
	for _, fh := range destinationHandles {
		fh.detached = true
	}
	if id, ok := fs.pathToInode[oldPath]; ok {
		delete(fs.pathToInode, oldPath)
		if ref := fs.inodes[id]; ref != nil {
			ref.Path = newPath
		}
		fs.pathToInode[newPath] = id
	}
	fs.mu.Unlock()
	unlockHandles()
	fs.closeCachedFilesForPath(oldPath)
	return nil
}

// maxSymlinkTargetBytes caps the size of a symlink target we'll hand back to
// the kernel. Linux PATH_MAX is 4096, which is the largest target a real
// symlink can point at anyway.
const maxSymlinkTargetBytes = 4096

func (fs *ArtifactFuse) ReadSymlink(ctx context.Context, op *fuseops.ReadSymlinkOp) error {
	ref, err := fs.requireInode(op.Inode, syscall.ESTALE)
	if err != nil {
		return err
	}
	n, err := fs.resolver.ResolvePath(ref.Path)
	if err != nil {
		return syscall.ENOENT
	}
	if n.Base.ObjectOID != "" {
		if err := validateKnownSymlinkTargetSize(n.Base); err != nil {
			return err
		}
		data, err := fs.engine.Hydrator.ReadBlob(ctx, fs.repo, n.Base, maxSymlinkTargetBytes)
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
	if node.SizeBytes > maxSymlinkTargetBytes {
		return syscall.ENAMETOOLONG
	}
	return nil
}

func (fs *ArtifactFuse) FlushFile(_ context.Context, _ *fuseops.FlushFileOp) error {
	return nil
}

func (fs *ArtifactFuse) SyncFile(ctx context.Context, op *fuseops.SyncFileOp) error {
	fh, err := fs.fileHandle(op.Handle)
	if err != nil {
		return err
	}
	path := fh.pathSnapshot()
	if path == ".git" {
		return nil
	}
	if pinned, err := fh.syncPinned(); pinned {
		if err != nil {
			return syscall.EIO
		}
		return nil
	}
	if err := fs.engine.Sync(ctx, path); err != nil {
		return syscall.EIO
	}
	return nil
}

func (fs *ArtifactFuse) ReleaseFileHandle(_ context.Context, op *fuseops.ReleaseFileHandleOp) error {
	fs.handleMu.Lock()
	defer fs.handleMu.Unlock()
	fs.mu.Lock()
	fh := fs.fileHandles[op.Handle]
	delete(fs.fileHandles, op.Handle)
	if fh != nil {
		delete(fs.filesByPath[fh.path], op.Handle)
		if len(fs.filesByPath[fh.path]) == 0 {
			delete(fs.filesByPath, fh.path)
		}
	}
	fs.mu.Unlock()
	if fh != nil {
		fh.closeFiles()
	}
	return nil
}

func (fs *ArtifactFuse) Destroy() {
	fs.handleMu.Lock()
	defer fs.handleMu.Unlock()
	fs.mu.Lock()
	handles := make([]*FileHandle, 0, len(fs.fileHandles))
	for _, fh := range fs.fileHandles {
		handles = append(handles, fh)
	}
	fs.fileHandles = make(map[fuseops.HandleID]*FileHandle)
	fs.filesByPath = make(map[string]map[fuseops.HandleID]*FileHandle)
	fs.mu.Unlock()
	for _, fh := range handles {
		fh.closeFiles()
	}
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
		EnableReaddirplus:       true,
		EnableAutoReaddirplus:   true,
	}
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
