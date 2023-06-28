package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
	"github.com/kelseyhightower/envconfig"
	log "github.com/sirupsen/logrus"
)

var Options struct {
	DataDir       string `envconfig:"DATA_DIR"`
	HTTPSKeyFile  string `envconfig:"HTTPS_KEY_FILE"`
	HTTPSCertFile string `envconfig:"HTTPS_CERT_FILE"`
	Port          string `envconfig:"PORT" default:"8080"`
}

func main() {
	log.SetReportCaller(true)
	err := envconfig.Process("fileserver", &Options)
	if err != nil {
		log.Fatalf("Failed to process config: %v\n", err)
	}

	inDir := filepath.Join(Options.DataDir, "input")
	isosDir := filepath.Join(Options.DataDir, "isos")

	files, err := os.ReadDir(inDir)
	if err != nil {
		log.Fatal(err)
	}

	for _, file := range files {
		if !file.IsDir() {
			fmt.Printf("skipping %s\n", file)
			continue
		}
		dirName := file.Name()
		isoPath := filepath.Join(isosDir, fmt.Sprintf("%s.iso", dirName))
		if err := create(isoPath, filepath.Join(inDir, dirName), dirName); err != nil {
			log.WithError(err).Fatalf("failed to create iso")
		}
		log.Infof("ISO created at %s", isoPath)
	}

	http.Handle("/", http.FileServer(http.Dir(isosDir)))
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	server := &http.Server{
		Addr: fmt.Sprintf(":%s", Options.Port),
	}
	go initServer(server, Options.HTTPSKeyFile, Options.HTTPSCertFile)

	<-stop
	if err := server.Shutdown(context.TODO()); err != nil {
		log.WithError(err).Errorf("shutdown failed: %v", err)
		if err := server.Close(); err != nil {
			log.WithError(err).Fatalf("emergency shutdown failed")
		}
	} else {
		log.Infof("server terminated gracefully")
	}
}

// create builds an iso file at outPath with the given volumeLabel using the contents of the working directory
func create(outPath string, workDir string, volumeLabel string) error {
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

func initServer(server *http.Server, httpsKeyFile, httpsCertFile string) {
	var err error
	if httpsKeyFile != "" && httpsCertFile != "" {
		log.Infof("Starting https handler on %s...", server.Addr)
		err = server.ListenAndServeTLS(httpsCertFile, httpsKeyFile)
	} else {
		log.Infof("Starting http handler on %s...", server.Addr)
		err = server.ListenAndServe()
	}

	if err != http.ErrServerClosed {
		log.WithError(err).Fatalf("HTTP listener closed: %v", err)
	}
}