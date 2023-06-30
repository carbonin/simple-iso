package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
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
	"github.com/stmcginnis/gofish"
	"github.com/stmcginnis/gofish/redfish"
)

var Options struct {
	DataDir       string `envconfig:"DATA_DIR"`
	HTTPSKeyFile  string `envconfig:"HTTPS_KEY_FILE"`
	HTTPSCertFile string `envconfig:"HTTPS_CERT_FILE"`
	Port          string `envconfig:"PORT" default:"8080"`
	BaseURL       string `envconfig:"BASE_URL"`

	BMCAddress  string `envconfig:"BMC_ADDRESS"`
	BMCPassword string `envconfig:"BMC_PASSWORD"`
	BMCUser     string `envconfig:"BMC_USER"`
}

func main() {
	log.SetReportCaller(true)
	err := envconfig.Process("fileserver", &Options)
	if err != nil {
		log.Fatalf("Failed to process config: %v\n", err)
	}

	// directory for fileserver and for isos to be created in
	isosDir := filepath.Join(Options.DataDir, "isos")
	if err := os.MkdirAll(isosDir, 0755); err != nil && !os.IsExist(err) {
		log.WithError(err).Fatal("failed to create iso output dir")
	}

	// create a single ISO to serve
	isoPath := filepath.Join(isosDir, "test-config.iso")
	isoWorkDir := filepath.Join(Options.DataDir, "input", "test-config")
	if err := os.MkdirAll(isoWorkDir, 0755); err != nil && !os.IsExist(err) {
		log.WithError(err).Fatal("failed to create iso work dir")
	}
	if err := createInputData(isoWorkDir); err != nil {
		log.WithError(err).Fatal("failed to write input data")
	}
	if err := create(isoPath, isoWorkDir, "test-config"); err != nil {
		log.WithError(err).Fatal("failed to create iso")
	}
	log.Infof("ISO created at %s", isoPath)

	http.Handle("/", http.FileServer(http.Dir(isosDir)))
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	server := &http.Server{
		Addr: fmt.Sprintf(":%s", Options.Port),
	}
	go initServer(server, Options.HTTPSKeyFile, Options.HTTPSCertFile)

	if Options.BMCAddress != "" {
		if err := configureBMC(Options.BMCAddress, Options.BMCUser, Options.BMCPassword, Options.BaseURL, "test-config.iso"); err != nil {
			log.Error(err)
		}
	}

	<-stop
	if err := server.Shutdown(context.TODO()); err != nil {
		log.WithError(err).Errorf("shutdown failed: %v", err)
		if err := server.Close(); err != nil {
			log.WithError(err).Fatal("emergency shutdown failed")
		}
	} else {
		log.Infof("server terminated gracefully")
	}
}

func configureBMC(address, user, pass, baseURL, file string) error {
	// parse url and create full url to iso
	_, err := url.JoinPath(baseURL, file)
	if err != nil {
		return err
	}

	u, err := url.Parse(address)
	if err != nil {
		return err
	}

	// connect to BMC
	config := gofish.ClientConfig{
		Endpoint:   fmt.Sprintf("%s://%s", u.Scheme, u.Host),
		Username:   user,
		Password:   pass,
		BasicAuth:  true,
		DumpWriter: log.StandardLogger().Writer(),
	}
	client, err := gofish.Connect(config)
	if err != nil {
		return fmt.Errorf("failed to connect using config %+v: %s", config, err)
	}

	system, err := redfish.GetComputerSystem(client, u.Path)
	if err != nil {
		return err
	}

	var isoVirtMedia *redfish.VirtualMedia
	for _, m := range system.ManagedBy {
		manager, err := redfish.GetManager(client, m)
		if err != nil {
			return err
		}
		vms, err := manager.VirtualMedia()
		if err != nil {
			return err
		}
		for _, vm := range vms {
			for _, vmType := range vm.MediaTypes {
				if vmType == redfish.CDMediaType {
					isoVirtMedia = vm
					break
				}
			}
		}
	}

	if isoVirtMedia != nil {
		log.Infof("found virt media for ISO: %+v", isoVirtMedia)
	}

	// add virtual media
	return nil
}

func createInputData(dir string) error {
	return os.WriteFile(filepath.Join(dir, "config"), []byte("config-data"), 0644)
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
