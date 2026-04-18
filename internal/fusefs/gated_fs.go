//go:build !windows

package fusefs

import (
	"context"
	"syscall"

	"github.com/jacobsa/fuse/fuseops"
	"github.com/jacobsa/fuse/fuseutil"
)

type gatedFileSystem struct {
	next fuseutil.FileSystem
	gate *ReadyGate
}

func NewGatedFileSystem(next fuseutil.FileSystem, gate *ReadyGate) fuseutil.FileSystem {
	if gate == nil {
		return next
	}
	return &gatedFileSystem{next: next, gate: gate}
}

func (fs *gatedFileSystem) wait(ctx context.Context) error {
	if err := fs.gate.Wait(ctx); err != nil {
		return syscall.EIO
	}
	return nil
}

func (fs *gatedFileSystem) StatFS(ctx context.Context, op *fuseops.StatFSOp) error {
	return fs.next.StatFS(ctx, op)
}

func (fs *gatedFileSystem) LookUpInode(ctx context.Context, op *fuseops.LookUpInodeOp) error {
	if err := fs.wait(ctx); err != nil {
		return err
	}
	return fs.next.LookUpInode(ctx, op)
}

func (fs *gatedFileSystem) GetInodeAttributes(ctx context.Context, op *fuseops.GetInodeAttributesOp) error {
	if op.Inode != fuseops.RootInodeID {
		if err := fs.wait(ctx); err != nil {
			return err
		}
	}
	return fs.next.GetInodeAttributes(ctx, op)
}

func (fs *gatedFileSystem) SetInodeAttributes(ctx context.Context, op *fuseops.SetInodeAttributesOp) error {
	if err := fs.wait(ctx); err != nil {
		return err
	}
	return fs.next.SetInodeAttributes(ctx, op)
}

func (fs *gatedFileSystem) ForgetInode(ctx context.Context, op *fuseops.ForgetInodeOp) error {
	return fs.next.ForgetInode(ctx, op)
}

func (fs *gatedFileSystem) BatchForget(ctx context.Context, op *fuseops.BatchForgetOp) error {
	return fs.next.BatchForget(ctx, op)
}

func (fs *gatedFileSystem) MkDir(ctx context.Context, op *fuseops.MkDirOp) error {
	if err := fs.wait(ctx); err != nil {
		return err
	}
	return fs.next.MkDir(ctx, op)
}

func (fs *gatedFileSystem) MkNode(ctx context.Context, op *fuseops.MkNodeOp) error {
	if err := fs.wait(ctx); err != nil {
		return err
	}
	return fs.next.MkNode(ctx, op)
}

func (fs *gatedFileSystem) CreateFile(ctx context.Context, op *fuseops.CreateFileOp) error {
	if err := fs.wait(ctx); err != nil {
		return err
	}
	return fs.next.CreateFile(ctx, op)
}

func (fs *gatedFileSystem) CreateLink(ctx context.Context, op *fuseops.CreateLinkOp) error {
	if err := fs.wait(ctx); err != nil {
		return err
	}
	return fs.next.CreateLink(ctx, op)
}

func (fs *gatedFileSystem) CreateSymlink(ctx context.Context, op *fuseops.CreateSymlinkOp) error {
	if err := fs.wait(ctx); err != nil {
		return err
	}
	return fs.next.CreateSymlink(ctx, op)
}

func (fs *gatedFileSystem) Rename(ctx context.Context, op *fuseops.RenameOp) error {
	if err := fs.wait(ctx); err != nil {
		return err
	}
	return fs.next.Rename(ctx, op)
}

func (fs *gatedFileSystem) RmDir(ctx context.Context, op *fuseops.RmDirOp) error {
	if err := fs.wait(ctx); err != nil {
		return err
	}
	return fs.next.RmDir(ctx, op)
}

func (fs *gatedFileSystem) Unlink(ctx context.Context, op *fuseops.UnlinkOp) error {
	if err := fs.wait(ctx); err != nil {
		return err
	}
	return fs.next.Unlink(ctx, op)
}

func (fs *gatedFileSystem) OpenDir(ctx context.Context, op *fuseops.OpenDirOp) error {
	if err := fs.wait(ctx); err != nil {
		return err
	}
	return fs.next.OpenDir(ctx, op)
}

func (fs *gatedFileSystem) ReadDir(ctx context.Context, op *fuseops.ReadDirOp) error {
	if err := fs.wait(ctx); err != nil {
		return err
	}
	return fs.next.ReadDir(ctx, op)
}

func (fs *gatedFileSystem) ReadDirPlus(ctx context.Context, op *fuseops.ReadDirPlusOp) error {
	if err := fs.wait(ctx); err != nil {
		return err
	}
	return fs.next.ReadDirPlus(ctx, op)
}

func (fs *gatedFileSystem) ReleaseDirHandle(ctx context.Context, op *fuseops.ReleaseDirHandleOp) error {
	return fs.next.ReleaseDirHandle(ctx, op)
}

func (fs *gatedFileSystem) OpenFile(ctx context.Context, op *fuseops.OpenFileOp) error {
	if err := fs.wait(ctx); err != nil {
		return err
	}
	return fs.next.OpenFile(ctx, op)
}

func (fs *gatedFileSystem) ReadFile(ctx context.Context, op *fuseops.ReadFileOp) error {
	if err := fs.wait(ctx); err != nil {
		return err
	}
	return fs.next.ReadFile(ctx, op)
}

func (fs *gatedFileSystem) WriteFile(ctx context.Context, op *fuseops.WriteFileOp) error {
	if err := fs.wait(ctx); err != nil {
		return err
	}
	return fs.next.WriteFile(ctx, op)
}

func (fs *gatedFileSystem) SyncFile(ctx context.Context, op *fuseops.SyncFileOp) error {
	return fs.next.SyncFile(ctx, op)
}

func (fs *gatedFileSystem) FlushFile(ctx context.Context, op *fuseops.FlushFileOp) error {
	return fs.next.FlushFile(ctx, op)
}

func (fs *gatedFileSystem) ReleaseFileHandle(ctx context.Context, op *fuseops.ReleaseFileHandleOp) error {
	return fs.next.ReleaseFileHandle(ctx, op)
}

func (fs *gatedFileSystem) ReadSymlink(ctx context.Context, op *fuseops.ReadSymlinkOp) error {
	if err := fs.wait(ctx); err != nil {
		return err
	}
	return fs.next.ReadSymlink(ctx, op)
}

func (fs *gatedFileSystem) RemoveXattr(ctx context.Context, op *fuseops.RemoveXattrOp) error {
	if err := fs.wait(ctx); err != nil {
		return err
	}
	return fs.next.RemoveXattr(ctx, op)
}

func (fs *gatedFileSystem) GetXattr(ctx context.Context, op *fuseops.GetXattrOp) error {
	if err := fs.wait(ctx); err != nil {
		return err
	}
	return fs.next.GetXattr(ctx, op)
}

func (fs *gatedFileSystem) ListXattr(ctx context.Context, op *fuseops.ListXattrOp) error {
	if err := fs.wait(ctx); err != nil {
		return err
	}
	return fs.next.ListXattr(ctx, op)
}

func (fs *gatedFileSystem) SetXattr(ctx context.Context, op *fuseops.SetXattrOp) error {
	if err := fs.wait(ctx); err != nil {
		return err
	}
	return fs.next.SetXattr(ctx, op)
}

func (fs *gatedFileSystem) Fallocate(ctx context.Context, op *fuseops.FallocateOp) error {
	if err := fs.wait(ctx); err != nil {
		return err
	}
	return fs.next.Fallocate(ctx, op)
}

func (fs *gatedFileSystem) SyncFS(ctx context.Context, op *fuseops.SyncFSOp) error {
	if err := fs.wait(ctx); err != nil {
		return err
	}
	return fs.next.SyncFS(ctx, op)
}

func (fs *gatedFileSystem) Destroy() {
	fs.next.Destroy()
}
