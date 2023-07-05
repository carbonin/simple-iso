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
	"time"

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
	// parse url and create full url to iso
	isoURL, err := url.JoinPath(Options.BaseURL, "images", testISOName)
	if err != nil {
		log.Fatal(err)
	}
	log.Infof("got ISO URL: %s", isoURL)

	server := startHTTPServer(log, isosDir, Options.Port, Options.HTTPSKeyFile, Options.HTTPSCertFile)

	if Options.BMCAddress != "" {
		if err := testVirtualMedia(log, isoURL); err != nil {
			log.WithError(err).Errorf("failed to test virtual media")
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

// testVirtualMedia connects to the BMC using the fields of Options and inserts and removes the test ISO
func testVirtualMedia(log *logrus.Logger, isoURL string) error {
	bmcURL, err := url.Parse(Options.BMCAddress)
	if err != nil {
		return fmt.Errorf("failed to parse BMC Address %s: %w", Options.BMCAddress, err)
	}

	config := gofish.ClientConfig{
		Endpoint:   fmt.Sprintf("%s://%s", bmcURL.Scheme, bmcURL.Host),
		Username:   Options.BMCUser,
		Password:   Options.BMCPassword,
		BasicAuth:  true,
		DumpWriter: log.WriterLevel(logrus.DebugLevel),
	}
	client, err := gofish.Connect(config)
	if err != nil {
		return fmt.Errorf("failed to connect to BMC: %w", err)
	}

	system, err := redfish.GetComputerSystem(client, bmcURL.Path)
	if err != nil {
		return fmt.Errorf("failed to get computer system: %w", err)
	}

	var isoVM *redfish.VirtualMedia
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
					isoVM = vm
					break
				}
			}
		}
	}

	if isoVM == nil {
		return fmt.Errorf("failed to find CD type virtual media")
	}

	if isoVM.Inserted {
		if err := isoVM.EjectMedia(); err != nil {
			return fmt.Errorf("failed to eject media: %w", err)
		}
	}
	if err := isoVM.InsertMedia(isoURL, true, true); err != nil {
		return fmt.Errorf("failed to insert media: %w", err)
	}

	log.Info("media inserted, booting host")

	if err := system.Reset(redfish.OnResetType); err != nil {
		return fmt.Errorf("failed to boot system: %w", err)
	}

	log.Info("waiting 5 minutes")
	time.Sleep(5 * time.Minute)

	if err := isoVM.EjectMedia(); err != nil {
		return fmt.Errorf("failed to eject media: %w", err)
	}
	log.Info("media ejected")

	return nil
}

func startHTTPServer(log *logrus.Logger, isosDir, port, httpsKeyFile, httpsCertFile string) *http.Server {
	http.Handle("/images/", http.StripPrefix("/images/", http.FileServer(http.Dir(isosDir))))
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
