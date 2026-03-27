package fusefs

import (
	"github.com/jacobsa/fuse"
)

func platformMountConfig(cfg *fuse.MountConfig) {
	cfg.FuseImpl = fuse.FUSEImplMacFUSE
}
