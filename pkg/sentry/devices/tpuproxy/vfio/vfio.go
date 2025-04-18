// Copyright 2023 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package vfio

import (
	"path"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/devutil"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/fdnotifier"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sync"
)

const (
	// VFIO_MINOR is the VFIO minor number from include/linux/miscdevice.h.
	VFIO_MINOR = 196

	tpuDeviceGroupName  = "vfio"
	vfioDeviceGroupName = "vfio"

	// VFIOPath is the valid path to a VFIO device.
	VFIOPath = "/dev/vfio/vfio"
)

var (
	tpuDeviceMajor        uint32
	tpuDeviceMajorInit    sync.Once
	tpuDeviceMajorInitErr error
)

// device implements TPU's vfs.Device for /dev/vfio/[0-9]+
//
// +stateify savable
type tpuDevice struct {
	mu sync.Mutex `state:"nosave"`

	// minor is the device minor number.
	minor uint32
	// num is the number of the device in the dev filesystem (e.g /dev/vfio/0).
	num uint32
	// useDevGofer indicates whether to use device gofer to open the TPU device.
	useDevGofer bool
}

// Open implements vfs.Device.Open.
func (dev *tpuDevice) Open(ctx context.Context, mnt *vfs.Mount, d *vfs.Dentry, opts vfs.OpenOptions) (*vfs.FileDescription, error) {
	dev.mu.Lock()
	defer dev.mu.Unlock()

	devPath := filepath.Join("vfio", strconv.Itoa(int(dev.num)))
	hostFD, err := openHostFD(ctx, devPath, opts.Flags, dev.useDevGofer)
	if err != nil {
		return nil, err
	}

	fd := &tpuFD{
		hostFD: int32(hostFD),
		device: dev,
	}
	if err := fd.vfsfd.Init(fd, opts.Flags, mnt, d, &vfs.FileDescriptionOptions{
		UseDentryMetadata: true,
	}); err != nil {
		unix.Close(hostFD)
		return nil, err
	}
	if err := fdnotifier.AddFD(int32(hostFD), &fd.queue); err != nil {
		unix.Close(hostFD)
		return nil, err
	}
	fd.memmapFile.fd = fd
	return &fd.vfsfd, nil
}

// device implements vfs.Device for /dev/vfio/vfio.
//
// +stateify savable
type vfioDevice struct {
	// useDevGofer indicates whether to use device gofer to open the VFIO device.
	useDevGofer bool
}

// Open implements vfs.Device.Open.
func (dev *vfioDevice) Open(ctx context.Context, mnt *vfs.Mount, d *vfs.Dentry, opts vfs.OpenOptions) (*vfs.FileDescription, error) {
	devPath := filepath.Join("vfio", "vfio")
	hostFD, err := openHostFD(ctx, devPath, opts.Flags, dev.useDevGofer)
	if err != nil {
		return nil, err
	}
	fd := &vfioFD{
		hostFD: int32(hostFD),
		device: dev,
	}
	if err := fd.vfsfd.Init(fd, opts.Flags, mnt, d, &vfs.FileDescriptionOptions{
		UseDentryMetadata: true,
	}); err != nil {
		unix.Close(hostFD)
		return nil, err
	}
	if err := fdnotifier.AddFD(int32(hostFD), &fd.queue); err != nil {
		unix.Close(hostFD)
		return nil, err
	}
	fd.memmapFile.fd = fd
	return &fd.vfsfd, nil
}

// RegisterTPUDevice registers devices implemented by this package in vfsObj.
func RegisterTPUDevice(vfsObj *vfs.VirtualFilesystem, minor, deviceNum uint32, useDevGofer bool) error {
	major, err := GetTPUDeviceMajor(vfsObj)
	if err != nil {
		return err
	}
	if vfsObj.IsDeviceRegistered(vfs.CharDevice, major, minor) {
		return nil
	}
	return vfsObj.RegisterDevice(vfs.CharDevice, major, minor, &tpuDevice{
		minor:       minor,
		num:         deviceNum,
		useDevGofer: useDevGofer,
	}, &vfs.RegisterDeviceOptions{
		GroupName: tpuDeviceGroupName,
		Pathname:  path.Join("vfio", strconv.Itoa(int(deviceNum))),
		FilePerms: 0666,
	})
}

// RegisterVFIODevice registers VFIO devices that are implemented by this package in vfsObj.
func RegisterVFIODevice(vfsObj *vfs.VirtualFilesystem, useDevGofer bool) error {
	if vfsObj.IsDeviceRegistered(vfs.CharDevice, linux.MISC_MAJOR, VFIO_MINOR) {
		return nil
	}
	return vfsObj.RegisterDevice(vfs.CharDevice, linux.MISC_MAJOR, VFIO_MINOR, &vfioDevice{
		useDevGofer: useDevGofer,
	}, &vfs.RegisterDeviceOptions{
		GroupName: vfioDeviceGroupName,
		Pathname:  path.Join("vfio", "vfio"),
		FilePerms: 0666,
	})
}

func openHostFD(ctx context.Context, devName string, flags uint32, useDevGofer bool) (int, error) {
	if useDevGofer {
		client := devutil.GoferClientFromContext(ctx)
		if client == nil {
			log.Warningf("devutil.CtxDevGoferClient is not set")
			return -1, linuxerr.ENOENT
		}
		return client.OpenAt(ctx, devName, flags)
	}
	devPath := filepath.Join("/", "dev", devName)
	openFlags := int(flags&unix.O_ACCMODE | unix.O_NOFOLLOW)
	return unix.Openat(-1, devPath, openFlags, 0)
}

// GetTPUDeviceMajor returns the dynamically allocated major number for the vfio
// device.
func GetTPUDeviceMajor(vfsObj *vfs.VirtualFilesystem) (uint32, error) {
	tpuDeviceMajorInit.Do(func() {
		tpuDeviceMajor, tpuDeviceMajorInitErr = vfsObj.GetDynamicCharDevMajor()
	})
	return tpuDeviceMajor, tpuDeviceMajorInitErr
}
