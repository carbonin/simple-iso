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
	"github.com/sirupsen/logrus"
	"github.com/stmcginnis/gofish"
	"github.com/stmcginnis/gofish/redfish"
)

var Options struct {
	DataDir       string `envconfig:"DATA_DIR"`
	LogLevel      string `envconfig:"LOG_LEVEL" default:"info"`
	Port          string `envconfig:"PORT" default:"8080"`
	BaseURL       string `envconfig:"BASE_URL"`
	HTTPSKeyFile  string `envconfig:"HTTPS_KEY_FILE"`
	HTTPSCertFile string `envconfig:"HTTPS_CERT_FILE"`

	BMCAddress  string `envconfig:"BMC_ADDRESS"`
	BMCPassword string `envconfig:"BMC_PASSWORD"`
	BMCUser     string `envconfig:"BMC_USER"`
}

const testISOName = "test-config.iso"

func main() {
	log := logrus.New()
	log.SetReportCaller(true)
	err := envconfig.Process("fileserver", &Options)
	if err != nil {
		log.Fatalf("Failed to process config: %v\n", err)
	}

	level, err := logrus.ParseLevel(Options.LogLevel)
	if err != nil {
		log.Fatal(err)
	}
	log.SetLevel(level)

	// directory for fileserver and for isos to be created in
	isosDir := filepath.Join(Options.DataDir, "isos")
	if err := os.MkdirAll(isosDir, 0755); err != nil && !os.IsExist(err) {
		log.WithError(err).Fatal("failed to create iso output dir")
	}

	if err := createTestISO(log, Options.DataDir, filepath.Join(isosDir, testISOName)); err != nil {
		log.Fatal(err)
	}

	server := startFileServer(log, isosDir, Options.Port, Options.HTTPSKeyFile, Options.HTTPSCertFile)

	if Options.BMCAddress != "" {
		// parse url and create full url to iso
		isoURL, err := url.JoinPath(Options.BaseURL, testISOName)
		if err != nil {
			log.Fatal(err)
		}
		log.Infof("got ISO URL: %s", isoURL)
		if err := configureBMC(log, Options.BMCAddress, Options.BMCUser, Options.BMCPassword, isoURL); err != nil {
			log.Error(err)
		} else {
			log.Infof("BMC configured to use new ISO for virtual media and powered on")
		}
	}

	waitForShutDown(log, server)
}

// createTestISO creates a single ISO containing a single file at isoPath
// the temp dir is cleaned up by the ISO creation process
func createTestISO(log *logrus.Logger, dataDir, isoPath string) error {
	isoWorkDir, err := os.MkdirTemp(dataDir, "test-config")
	if err != nil {
		return fmt.Errorf("failed to create iso work dir: %w", err)
	}
	if err := createInputData(isoWorkDir); err != nil {
		return fmt.Errorf("failed to write input data: %w", err)
	}
	if err := create(isoPath, isoWorkDir, "test-config"); err != nil {
		return fmt.Errorf("failed to create iso: %w", err)
	}
	log.Infof("Test iso created at %s", isoPath)
	return nil
}

// createInputData writes a test file in dir to be packaged into an iso
// in the future more meaningful data should be included here
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

func configureBMC(log *logrus.Logger, address, user, pass, isoURL string) error {
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
		DumpWriter: log.WriterLevel(logrus.DebugLevel),
	}
	client, err := gofish.Connect(config)
	if err != nil {
		return err
	}

	system, err := redfish.GetComputerSystem(client, u.Path)
	if err != nil {
		return err
	}

	// find CD virtual media
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
	if isoVirtMedia == nil {
		return fmt.Errorf("failed to find CD type virtual media")
	}

	// add ISO to virtual media
	if isoVirtMedia.Inserted {
		if err := isoVirtMedia.EjectMedia(); err != nil {
			log.Error("failed to eject media")
			return err
		}
	}
	if err := isoVirtMedia.InsertMedia(isoURL, true, true); err != nil {
		return err
	}

	// boot
	return system.Reset(redfish.OnResetType)
}

func startFileServer(log *logrus.Logger, isosDir, port, httpsKeyFile, httpsCertFile string) *http.Server {
	http.Handle("/", http.FileServer(http.Dir(isosDir)))
	server := &http.Server{
		Addr: fmt.Sprintf(":%s", port),
	}

	go func() {
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
	}()

	return server
}

func waitForShutDown(log *logrus.Logger, server *http.Server) {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	if err := server.Shutdown(context.Background()); err != nil {
		log.WithError(err).Errorf("shutdown failed")
		if err := server.Close(); err != nil {
			log.WithError(err).Fatal("emergency shutdown failed")
		}
	} else {
		log.Infof("server terminated gracefully")
	}
}
