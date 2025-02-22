// This file is part of MinIO Direct CSI
// Copyright (c) 2021 MinIO, Inc.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package quota

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"syscall"
	"unsafe"

	simd "github.com/minio/sha256-simd"
	"k8s.io/klog/v2"
)

func getProjectIDHash(id string) uint32 {
	h := simd.Sum256([]byte(id))
	return binary.LittleEndian.Uint32(h[:8])
}

func setProjectID(path string, projectID uint32) error {

	targetDir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("could not open %v: %v", path, err)
	}
	defer targetDir.Close()

	var fsx fsXAttr
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		targetDir.Fd(),
		fsGetAttr,
		uintptr(unsafe.Pointer(&fsx))); errno != 0 {
		return fmt.Errorf("failed to execute GETFSXAttrs. path: %v error: %v", path, errno)
	}

	fsx.fsXProjID = uint32(projectID)
	fsx.fsXXFlags |= uint32(flagProjectInherit)
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		targetDir.Fd(),
		fsSetAttr,
		uintptr(unsafe.Pointer(&fsx))); errno != 0 {
		return fmt.Errorf("failed to execute SETFSXAttrs. path: %v projectID: %v error: %v", path, fsx.fsXProjID, errno)
	}

	return nil
}

func setProjectQuota(blockFile string, projectID uint32, quota FSQuota) error {

	hardLimitBlocks := uint64(math.Ceil(float64(quota.HardLimit) / blockSize))
	softLimitBlocks := uint64(math.Ceil(float64(quota.SoftLimit) / blockSize))

	fsQuota := &fsDiskQuota{
		version:         int8(fsDiskQuotaVersion),
		flags:           int8(xfsProjectQuotaFlag),
		fieldmask:       uint16(fieldMaskBHard | fieldMaskBSoft),
		id:              uint32(projectID),
		hardLimitBlocks: hardLimitBlocks,
		softLimitBlocks: softLimitBlocks,
	}

	deviceNamePtr, err := syscall.BytePtrFromString(blockFile)
	if err != nil {
		return err
	}
	if _, _, errno := syscall.Syscall6(syscall.SYS_QUOTACTL,
		prjSetQuotaLimit,
		uintptr(unsafe.Pointer(deviceNamePtr)),
		uintptr(projectID),
		uintptr(unsafe.Pointer(fsQuota)),
		0,
		0); errno != syscall.Errno(0) {
		return fmt.Errorf("failed to set quota for device: %s error: %v", blockFile, errno)
	}

	return nil
}

func GetQuota(blockFile, volumeID string) (FSQuota, error) {
	result := &fsDiskQuota{}
	deviceNamePtr, err := syscall.BytePtrFromString(blockFile)
	if err != nil {
		return FSQuota{}, err
	}
	projectID := int(getProjectIDHash(volumeID))

	if _, _, errno := syscall.RawSyscall6(syscall.SYS_QUOTACTL,
		prjGetQuota,
		uintptr(unsafe.Pointer(deviceNamePtr)),
		uintptr(projectID),
		uintptr(unsafe.Pointer(result)),
		0,
		0); errno != 0 {
		return FSQuota{}, os.NewSyscallError("quotactl", errno)
	}

	return FSQuota{
		HardLimit:    int64(result.hardLimitBlocks) * blockSize,
		SoftLimit:    int64(result.softLimitBlocks) * blockSize,
		CurrentSpace: int64(result.blocksCount) * blockSize,
	}, nil
}

func SetQuota(ctx context.Context, path, volumeID, blockFile string, quota FSQuota) error {
	_, err := GetQuota(blockFile, volumeID)
	// this means quota has already been set
	if err == nil {
		return nil
	}

	projectID := getProjectIDHash(volumeID)
	if err := setProjectID(path, projectID); err != nil {
		klog.Errorf("could not set projectID err=%v", err)
		return err
	}

	klog.V(3).InfoS("Setting projectquota",
		"VolumeID", volumeID,
		"ProjectID", projectID,
		"Path", path,
		"limit", quota.HardLimit)
	if err := setProjectQuota(blockFile, projectID, quota); err != nil {
		klog.Errorf("could not setquota err=%v", err)
		return err
	}
	klog.V(3).InfoS("Successfully set projectquota",
		"VolumeID", volumeID,
		"ProjectID", projectID)
	return nil
}
