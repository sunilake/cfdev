package cmd

import (
	"fmt"
	"os"
	"io/ioutil"
	"strconv"
	"flag"
	"time"
	"syscall"
	"strings"
	"net/url"
	gdn "code.cloudfoundry.org/cfdev/garden"
	"code.cloudfoundry.org/cfdev/network"
	"code.cloudfoundry.org/cfdev/process"
	"code.cloudfoundry.org/garden"
	"code.cloudfoundry.org/garden/client"
	"code.cloudfoundry.org/garden/client/connection"
	"code.cloudfoundry.org/cfdev/config"
	"code.cloudfoundry.org/cfdev/env"
)

type UI interface {
	Say(message string, args ...interface{})
}

type Start struct {
	Exit chan struct{}
	UI UI
	Config config.Config
}

func (s *Start) Run(args []string) error {
	startCmd := flag.NewFlagSet("start", flag.ExitOnError)
	registriesFlag := startCmd.String("r", "", "docker registries that skip ssl validation - ie. host:port,host2:port2")
	startCmd.Parse(args)

	if err := env.Setup(s.Config); err != nil {
		return err
	}

	if isLinuxKitRunning(s.Config.LinuxkitPidFile) {
		s.UI.Say("CF Dev is already running...")
		return nil
	}

	registries, err := s.parseDockerRegistriesFlag(*registriesFlag)
	if err != nil {
		fmt.Errorf("Unable to parse docker registries %v\n", err)
		os.Exit(1)
	}

	vpnKit := process.VpnKit{
		Config: s.Config,
	}
	vCmd := vpnKit.Command()

	linuxkit := process.LinuxKit{
		Config: s.Config,
	}

	lCmd := linuxkit.Command()

	go func() {
		<-s.Exit
		process.Terminate(s.Config.LinuxkitPidFile)
		process.Terminate(s.Config.VpnkitPidFile)
		process.Kill(s.Config.HyperkitPidFile)
		os.Exit(128)
	}()

	if err = cleanupStateDir(s.Config.StateDir); err != nil {
		return err
	}

	if err = s.setupNetworking(); err != nil {
		return err
	}

	s.UI.Say("Downloading Resources...")
	if err = download(s.Config.Dependencies, s.Config.CacheDir); err != nil {
		return err
	}

	if err = vpnKit.SetupVPNKit(); err != nil {
		return err
	}

	s.UI.Say("Starting VPNKit ...")
	if err := vCmd.Start(); err != nil {
		return fmt.Errorf("Failed to start VPNKit process: %v\n", err)
	}

	err = ioutil.WriteFile(s.Config.VpnkitPidFile, []byte(strconv.Itoa(vCmd.Process.Pid)), 0777)
	if err != nil {
		return fmt.Errorf("Failed to write vpnKit pid file: %v\n", err)
	}

	s.UI.Say("Starting the VM...")
	if err := lCmd.Start(); err != nil {
		return fmt.Errorf("Failed to start VM process: %v\n", err)
	}

	err = ioutil.WriteFile(s.Config.LinuxkitPidFile, []byte(strconv.Itoa(lCmd.Process.Pid)), 0777)
	if err != nil {
		return fmt.Errorf("Failed to write VM pid file: %v\n", err)
	}

	garden := client.New(connection.New("tcp", "localhost:8888"))
	waitForGarden(garden)

	s.UI.Say("Deploying the BOSH Director...")
	if err := gdn.DeployBosh(garden); err != nil {
		return fmt.Errorf("Failed to deploy the BOSH Director: %v\n", err)
	}

	s.UI.Say("Deploying CF...")
	if err := gdn.DeployCloudFoundry(garden, registries); err != nil {
		return fmt.Errorf("Failed to deploy the Cloud Foundry: %v\n", err)
	}

	s.UI.Say(`
  ██████╗███████╗██████╗ ███████╗██╗   ██╗
 ██╔════╝██╔════╝██╔══██╗██╔════╝██║   ██║
 ██║     █████╗  ██║  ██║█████╗  ██║   ██║
 ██║     ██╔══╝  ██║  ██║██╔══╝  ╚██╗ ██╔╝
 ╚██████╗██║     ██████╔╝███████╗ ╚████╔╝
  ╚═════╝╚═╝     ╚═════╝ ╚══════╝  ╚═══╝
             is now running!

To begin using CF Dev, please run:
    cf login -a https://api.v3.pcfdev.io --skip-ssl-validation

Admin user => Email: admin / Password: admin
Regular user => Email: user / Password: pass
`)
	return nil
}

func waitForGarden(client garden.Client) {
	for {
		if err := client.Ping(); err == nil {
			return
		}

		time.Sleep(time.Second)
	}
}

func isLinuxKitRunning(pidFile string) bool {
	fileBytes, err := ioutil.ReadFile(pidFile)
	if err != nil {
		return false
	}

	pid, err := strconv.ParseInt(string(fileBytes), 10, 64)
	if err != nil {
		return false
	}

	if process, err := os.FindProcess(int(pid)); err == nil {
		err = process.Signal(syscall.Signal(0))
		return err == nil
	}

	return false
}

func cleanupStateDir(stateDir string) error {
	if err := os.RemoveAll(stateDir); err != nil {
		return fmt.Errorf("Unable to clean up .cfdev state directory: %v\n", err)
	}

	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return fmt.Errorf("Unable to create .cfdev state directory: %v\n", err)
	}

	return nil
}

func(s *Start) setupNetworking() error {
	err := network.AddLoopbackAliases(s.Config.BoshDirectorIP, s.Config.CFRouterIP)

	if err != nil {
		return fmt.Errorf("Unable to alias BOSH Director/CF Router IP: %v\n", err)
	}

	return nil
}

func(s *Start) parseDockerRegistriesFlag(flag string) ([]string, error) {
	if flag == "" {
		return nil, nil
	}

	values := strings.Split(flag, ",")

	registries := make([]string, 0, len(values))

	for _, value := range values {
		// Including the // will cause url.Parse to validate 'value' as a host:port
		u, err := url.Parse("//" + value)

		if err != nil {
			// Grab the more succinct error message
			if urlErr, ok := err.(*url.Error); ok {
				err = urlErr.Err
			}
			return nil, fmt.Errorf("'%v' - %v", value, err)
		}
		registries = append(registries, u.Host)
	}
	return registries, nil
}
