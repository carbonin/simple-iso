package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
)

func main() {
	dir, err := os.Getwd()
	if err != nil {
		log.Fatalf("failed to get working directory: %s", err)
	}
	outPath := filepath.Join(dir, "isos/test.iso")
	err = Create(outPath, filepath.Join(dir, "files"), "config")
	if err != nil {
		log.Fatalf("failed to create iso: %s", err)
	}
	log.Printf("ISO created at %s", outPath)
}

// Create builds an iso file at outPath with the given volumeLabel using the contents of the working directory
func Create(outPath string, workDir string, volumeLabel string) error {
	// Use the minimum iso size that will satisfy diskfs validations here.
	// This value doesn't determine the final image size, but is used
	// to truncate the initial file. This value would be relevant if
	// we were writing to a particular partition on a device, but we are
	// not so the minimum iso size will work for us here
	minISOSize := 38 * 1024
	d, err := diskfs.Create(outPath, int64(minISOSize), diskfs.Raw, diskfs.SectorSizeDefault)
	if err != nil {
		return err
	}

	d.LogicalBlocksize = 2048
	fspec := disk.FilesystemSpec{
		Partition:   0,
		FSType:      filesystem.TypeISO9660,
		VolumeLabel: volumeLabel,
		WorkDir:     workDir,
	}
	fs, err := d.CreateFilesystem(fspec)
	if err != nil {
		return err
	}

	iso, ok := fs.(*iso9660.FileSystem)
	if !ok {
		return fmt.Errorf("not an iso9660 filesystem")
	}

	options := iso9660.FinalizeOptions{
		RockRidge:        true,
		VolumeIdentifier: volumeLabel,
	}

	return iso.Finalize(options)
}
