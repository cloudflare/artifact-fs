package fusefs

import (
	"github.com/jacobsa/fuse"
)

func platformMountConfig(cfg *fuse.MountConfig) {
	// FUSEImplFuseT is the zero value and the correct choice for Linux.
	cfg.FuseImpl = fuse.FUSEImplFuseT
}
