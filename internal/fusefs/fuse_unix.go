//go:build !windows

package fusefs

import (
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
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
	inodes       map[fuseops.InodeID]*InodeRef
	pathToInode  map[string]fuseops.InodeID
	nextInodeID  fuseops.InodeID
	dirHandles   map[fuseops.HandleID]*DirHandle
	fileHandles  map[fuseops.HandleID]*FileHandle
	nextHandleID fuseops.HandleID
}

type InodeRef struct {
	ID      fuseops.InodeID
	Path    string
	Type    string // file, dir, symlink
	Mode    uint32
	Refcnt  int64
	IsRoot  bool
	Overlay bool
}

type DirHandle struct {
	inode   *InodeRef
	entries []ReaddirEntry
}

type FileHandle struct {
	inode *InodeRef
	path  string
}

// ReaddirEntry holds a child name and type, avoiding per-child Getattr calls.
type ReaddirEntry struct {
	Name string
	Type string // file, dir, symlink
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

func (fs *ArtifactFuse) allocInode(path, typ string, mode uint32) *InodeRef {
	// Caller must hold fs.mu write lock.
	if id, ok := fs.pathToInode[path]; ok {
		if ref, ok := fs.inodes[id]; ok {
			ref.Refcnt++
			return ref
		}
	}
	id := fs.nextInodeID
	fs.nextInodeID++
	ref := &InodeRef{ID: id, Path: path, Type: typ, Mode: mode, Refcnt: 1}
	fs.inodes[id] = ref
	fs.pathToInode[path] = id
	return ref
}

func (fs *ArtifactFuse) getInode(id fuseops.InodeID) *InodeRef {
	fs.mu.RLock()
	ref := fs.inodes[id]
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

func (fs *ArtifactFuse) LookUpInode(_ context.Context, op *fuseops.LookUpInodeOp) error {
	parent, err := fs.requireInode(op.Parent, syscall.ENOENT)
	if err != nil {
		return err
	}

	childPath := cleanChildPath(parent.Path, op.Name)

	// Synthesize .git gitfile in root
	if parent.IsRoot && op.Name == ".git" {
		fs.mu.Lock()
		ref := fs.allocInode(".git", "file", 0o644)
		fs.mu.Unlock()
		op.Entry.Child = ref.ID
		op.Entry.Attributes = fs.gitFileAttrs()
		setChildEntryExpiry(&op.Entry, time.Minute)
		return nil
	}

	mode, size, typ, mtime, err := fs.resolver.Getattr(childPath)
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return syscall.ENOENT
		}
		return syscall.EIO
	}

	fs.mu.Lock()
	ref := fs.allocInode(childPath, typ, mode)
	fs.mu.Unlock()

	op.Entry.Child = ref.ID
	op.Entry.Attributes = inodeAttrs(mode, uint64(size), typ, mtime)
	setChildEntryExpiry(&op.Entry, time.Second)
	return nil
}

func (fs *ArtifactFuse) GetInodeAttributes(_ context.Context, op *fuseops.GetInodeAttributesOp) error {
	ref, err := fs.requireInode(op.Inode, syscall.ESTALE)
	if err != nil {
		return err
	}

	if ref.IsRoot {
		op.Attributes = inodeAttrs(ref.Mode, 4096, "dir", time.Now())
		op.AttributesExpiration = attrExpiry(time.Second)
		return nil
	}

	if ref.Path == ".git" {
		op.Attributes = fs.gitFileAttrs()
		op.AttributesExpiration = attrExpiry(time.Minute)
		return nil
	}

	mode, size, typ, mtime, err := fs.resolver.Getattr(ref.Path)
	if err != nil {
		return syscall.ENOENT
	}
	op.Attributes = inodeAttrs(mode, uint64(size), typ, mtime)
	op.AttributesExpiration = attrExpiry(time.Second)
	return nil
}

func (fs *ArtifactFuse) SetInodeAttributes(ctx context.Context, op *fuseops.SetInodeAttributesOp) error {
	ref, err := fs.requireInode(op.Inode, syscall.ESTALE)
	if err != nil {
		return err
	}
	if op.Size != nil {
		if err := fs.engine.Truncate(ctx, ref.Path, int64(*op.Size)); err != nil {
			return syscall.EIO
		}
	}
	// Handle mtime updates (e.g., from touch)
	if op.Mtime != nil {
		fs.engine.SetMtime(ctx, ref.Path, *op.Mtime)
	}
	mode, size, typ, mtime, err := fs.resolver.Getattr(ref.Path)
	if err != nil {
		return syscall.EIO
	}
	// If caller set mtime, return that instead of the stored value
	if op.Mtime != nil {
		mtime = *op.Mtime
	}
	op.Attributes = inodeAttrs(mode, uint64(size), typ, mtime)
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
			delete(fs.pathToInode, ref.Path)
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
	// Eagerly load children at open time to avoid races on concurrent ReadDir.
	entries, err := fs.resolver.ReaddirTyped(ctx, ref.Path)
	if err != nil {
		return syscall.EIO
	}
	if ref.IsRoot {
		entries = append([]ReaddirEntry{{Name: ".git", Type: "file"}}, entries...)
	}

	// Speculative prefetch: enqueue file children for hydration at a lower
	// priority so they're warmed in the cache before the user opens them.
	go fs.engine.PrefetchDir(ref.Path, entries)

	dh := &DirHandle{inode: ref, entries: entries}
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
		dt := fuseutil.DT_File
		switch e.Type {
		case "dir":
			dt = fuseutil.DT_Directory
		case "symlink":
			dt = fuseutil.DT_Link
		}
		dirent := fuseutil.Dirent{
			Offset: fuseops.DirOffset(i + 1),
			Inode:  fuseops.RootInodeID + 1, // placeholder; kernel re-looks-up via LookUpInode
			Name:   e.Name,
			Type:   dt,
		}
		n := fuseutil.WriteDirent(op.Dst[op.BytesRead:], dirent)
		if n == 0 {
			break
		}
		op.BytesRead += n
	}
	return nil
}

func (fs *ArtifactFuse) ReleaseDirHandle(_ context.Context, op *fuseops.ReleaseDirHandleOp) error {
	fs.mu.Lock()
	delete(fs.dirHandles, op.Handle)
	fs.mu.Unlock()
	return nil
}

func (fs *ArtifactFuse) OpenFile(_ context.Context, op *fuseops.OpenFileOp) error {
	ref, err := fs.requireInode(op.Inode, syscall.ESTALE)
	if err != nil {
		return err
	}
	fh := &FileHandle{inode: ref, path: ref.Path}
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
	fh, err := fs.fileHandle(op.Handle)
	if err != nil {
		return err
	}

	if fh.path == ".git" {
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

	data, err := fs.engine.Read(ctx, fh.path, op.Offset, int(op.Size))
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
	fh, err := fs.fileHandle(op.Handle)
	if err != nil {
		return err
	}
	_, err = fs.engine.Write(ctx, fh.path, op.Offset, op.Data)
	if err != nil {
		return syscall.EIO
	}
	return nil
}

func (fs *ArtifactFuse) CreateFile(ctx context.Context, op *fuseops.CreateFileOp) error {
	_, childPath, err := fs.childPath(op.Parent, op.Name)
	if err != nil {
		return err
	}
	if err := fs.engine.Create(ctx, childPath, uint32(op.Mode)); err != nil {
		return syscall.EIO
	}
	fs.mu.Lock()
	ref := fs.allocInode(childPath, "file", uint32(op.Mode))
	fh := &FileHandle{inode: ref, path: childPath}
	handle := fs.nextHandleID
	fs.nextHandleID++
	fs.fileHandles[handle] = fh
	fs.mu.Unlock()

	op.Entry.Child = ref.ID
	op.Entry.Attributes = inodeAttrs(uint32(op.Mode), 0, "file", time.Now())
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
	ref := fs.allocInode(childPath, "dir", uint32(op.Mode))
	fs.mu.Unlock()

	op.Entry.Child = ref.ID
	op.Entry.Attributes = inodeAttrs(uint32(op.Mode)|uint32(os.ModeDir), 4096, "dir", time.Now())
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
	_, childPath, err := fs.childPath(op.Parent, op.Name)
	if err != nil {
		return err
	}
	if err := fs.engine.Unlink(ctx, childPath); err != nil {
		return syscall.EIO
	}
	return nil
}

func (fs *ArtifactFuse) Rename(ctx context.Context, op *fuseops.RenameOp) error {
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
	if err := fs.engine.Rename(ctx, oldPath, newPath); err != nil {
		return syscall.EIO
	}
	return nil
}

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
		cachePath, _, err := fs.engine.Hydrator.EnsureHydrated(ctx, fs.repo, n.Base)
		if err != nil {
			return syscall.EIO
		}
		data, err := os.ReadFile(cachePath)
		if err != nil {
			return syscall.EIO
		}
		op.Target = string(data)
		return nil
	}
	return syscall.ENOENT
}

func (fs *ArtifactFuse) FlushFile(_ context.Context, _ *fuseops.FlushFileOp) error {
	return nil
}

func (fs *ArtifactFuse) SyncFile(_ context.Context, _ *fuseops.SyncFileOp) error {
	return nil
}

func (fs *ArtifactFuse) ReleaseFileHandle(_ context.Context, op *fuseops.ReleaseFileHandleOp) error {
	fs.mu.Lock()
	delete(fs.fileHandles, op.Handle)
	fs.mu.Unlock()
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
		// UseReadDirPlus intentionally not set -- the ReadDir implementation
		// uses WriteDirent (plain format). Enable only after implementing
		// WriteDirentPlus with full ChildInodeEntry.
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

func inodeAttrs(mode uint32, size uint64, typ string, mtime time.Time) fuseops.InodeAttributes {
	m := os.FileMode(mode & 0o777)
	if m == 0 {
		if typ == "dir" {
			m = 0o755
		} else {
			m = 0o644
		}
	}
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
		Mtime: mtime,
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
	return fuseops.InodeAttributes{
		Size:  uint64(len(fs.gitfileContent)),
		Mode:  0o644,
		Nlink: 1,
		Uid:   uint32(os.Getuid()),
		Gid:   uint32(os.Getgid()),
		Mtime: time.Now(),
	}
}
