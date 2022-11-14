package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	clconfig "github.com/flatcar-linux/container-linux-config-transpiler/config"
	"github.com/hetznercloud/hcloud-go/hcloud"
	"github.com/melbahja/goph"
	"gopkg.in/yaml.v3"
)

var installScriptSource = "https://raw.githubusercontent.com/flatcar-linux/init/flatcar-master/bin/flatcar-install"

func transpileConfig(input []byte) (string, error) {
	cfg, pt, report := clconfig.Parse(input)
	if report.IsFatal() {
		return "", errors.New("config parsing failed")
	}
	transpiledConfig, report := clconfig.Convert(cfg, "", pt)
	if report.IsFatal() {
		return "", errors.New("config conversion failed")
	}
	cfgJSON, err := json.Marshal(&transpiledConfig)
	if err != nil {
		return "", err
	}

	outFile, err := os.CreateTemp(os.TempDir(), "ignition")
	if err != nil {
		return "", err
	}

	if _, err := outFile.Write(cfgJSON); err != nil {
		return "", err
	}
	return outFile.Name(), nil
}

// waitForAction queries the current state of an action every second and waits for it to complete
func waitForAction(actionClient hcloud.ActionClient, action *hcloud.Action) error {
	log.Printf("waiting for action %s to complete\n", action.Command)
	progressChannel, errorChannel := actionClient.WatchProgress(context.Background(), action)
	success := false
	for progress := range progressChannel {
		if progress == 100 {
			success = true
		}
	}
	var err error
	if !success {
		// channel was closed before progress was 100 so there was probably an error
		err = <-errorChannel
	}
	return err
}

type templateData struct {
	Server   hcloud.Server
	SSHKey   hcloud.SSHKey
	Static   map[string]string
	ReadFile func(string) (string, error)
	Indent   func(int, string) string
}

type customTemplateDataHetzner struct {
	Server hcloud.Server
	SSHKey hcloud.SSHKey
}

type customTemplateData struct {
	Hetzner customTemplateDataHetzner
}

func main() {
	// TODO: cli parser...
	if len(os.Args) == 1 {
		fmt.Printf("%s <server name>\n", os.Args[0])
		os.Exit(1)
	}
	cfg, err := ParseConfig("config.toml")
	if err != nil {
		log.Fatalf("error parsing config: %v\n", err)
	}

	serverName := os.Args[1]

	client := hcloud.NewClient(hcloud.WithToken(cfg.HCloud.Token))

	// find ssh key
	sshKeyName := cfg.HCloud.SSHKey
	sshKey, _, err := client.SSHKey.GetByName(context.Background(), sshKeyName)
	if err != nil {
		log.Fatalf("error requesting ssh key: %v\n", err)
	}
	if sshKey == nil {
		log.Fatalf("ssh key %s doesn't exist\n", sshKeyName)
	}

	// find private network
	privateNetworkName := cfg.HCloud.PrivateNetwork
	privateNetwork, _, err := client.Network.GetByName(context.Background(), privateNetworkName)
	if err != nil {
		log.Fatalf("error requesting network: %v\n", err)
	}
	if privateNetwork == nil {
		log.Fatalf("network %s doesn't exist\n", privateNetworkName)
	}

	serverExists := true
	server, _, err := client.Server.GetByName(context.Background(), serverName)
	if err != nil {
		log.Fatalf("error finding server: %v\n", err)
	}
	if server == nil {
		serverExists = false
	}

	if serverExists {
		log.Printf("server '%s' (id %d) already exists, checking for necessary changes\n", serverName, server.ID)
		// check if redeploy is necessary -- fetching user data afterwards not possible, maybe cache locally/connect to server?
		// TODO: check if specification matches
		// TODO: support more than one network?
		// TODO: disable if network doesn't exist / not given
		privateNetworkAttached := false
		for _, attachedPrivateNet := range server.PrivateNet {
			if attachedPrivateNet.Network.ID == privateNetwork.ID {
				privateNetworkAttached = true
				break
			}
		}
		if !privateNetworkAttached {
			// attach to private network
			action, _, err := client.Server.AttachToNetwork(context.Background(), server, hcloud.ServerAttachToNetworkOpts{
				Network: privateNetwork,
			})
			if err != nil {
				log.Fatalf("error request attach to network: %v\n", err)
			}
			if action.Error() != nil {
				log.Fatalf("error attaching server to network: %v\n", action.Error())
			}
			log.Printf("attached server to network %s\n", privateNetworkName)
		}
	} else {
		log.Printf("creating server '%s'", serverName)
		// create server
		startAfterCreate := false
		serverType, _, err := client.ServerType.GetByName(context.Background(), cfg.HCloud.ServerType)
		if err != nil {
			log.Fatalf("error finding server type: %v\n", err)
		}
		image, _, err := client.Image.Get(context.Background(), cfg.HCloud.Image)
		if err != nil {
			log.Fatalf("error finding image: %v\n", err)
		}
		location, _, err := client.Location.GetByName(context.Background(), cfg.HCloud.Location)
		if err != nil {
			log.Fatalf("error finding location: %v\n", err)
		}
		createOpts := hcloud.ServerCreateOpts{
			Name:             serverName,
			StartAfterCreate: &startAfterCreate,
			ServerType:       serverType,
			Image:            image,
			Location:         location,
			SSHKeys:          []*hcloud.SSHKey{sshKey},
			Networks:         []*hcloud.Network{privateNetwork},
		}
		serverCreateResult, _, err := client.Server.Create(context.Background(), createOpts)
		if err != nil {
			log.Fatalf("error creating server: %v\n", err)
		}
		if serverCreateResult.Action.Error() != nil {
			log.Fatalf("error creating server: %v\n", serverCreateResult.Action.Error())
		}

		err = waitForAction(client.Action, serverCreateResult.Action)
		if err != nil {
			log.Fatalf("error waiting for action: %v\n", err)
		}

		for _, pastCreateAction := range serverCreateResult.NextActions {
			err = waitForAction(client.Action, pastCreateAction)
			if err != nil {
				log.Fatalf("error waiting for action: %v\n", err)
			}
		}

		// update server object for templating
		server, _, err = client.Server.GetByID(context.Background(), serverCreateResult.Server.ID)
		if err != nil {
			log.Fatalf("error requesting updated server object: %v\n", err)
		}
	}

	var templateContent []byte
	if cfg.Flatcar.TemplateCommand == "" {
		ignitionTemplate := cfg.Flatcar.ConfigTemplate
		log.Printf("rendering ignition config using native template at %s\n", ignitionTemplate)
		buffer := &bytes.Buffer{}
		tmpl, err := template.New(filepath.Base(ignitionTemplate)).ParseFiles(ignitionTemplate)
		if err != nil {
			log.Fatalf("error loading template: %v\n", err)
		}
		err = tmpl.Execute(buffer, templateData{
			Server: *server,
			SSHKey: *sshKey,
			Static: cfg.Flatcar.TemplateStatic,
			ReadFile: func(filename string) (string, error) {
				content, err := ioutil.ReadFile(filename)
				return string(content), err
			},
			Indent: func(indent int, input string) string {
				lines := strings.Split(input, "\n")
				output := make([]string, len(lines))
				indentString := strings.Repeat(" ", indent)
				for i := 0; i < len(output); i++ {
					output[i] = indentString + lines[i]
				}
				return strings.Join(output, "\n")
			},
		})
		if err != nil {
			log.Fatalf("error rendering template: %v\n", err)
		}

		templateContent, _ = ioutil.ReadAll(buffer)
	} else {
		log.Printf("rendering ignition config using command '%s'\n", cfg.Flatcar.TemplateCommand)

		// marshal template data for passing it to the custom command
		templateData := customTemplateData{
			Hetzner: customTemplateDataHetzner{
				Server: *server,
				SSHKey: *sshKey,
			},
		}
		templateDataYAML, err := yaml.Marshal(templateData)
		if err != nil {
			log.Fatalf("error marshaling hcloud data to yaml: %v\n", err)
		}

		// execute custom template command
		tmplCmd := exec.Command(cfg.Flatcar.TemplateCommand, server.Name)
		tmplCmd.Stdin = bytes.NewReader(templateDataYAML)
		templateContent, err = tmplCmd.Output()
		if err != nil {
			log.Println(string(err.(*exec.ExitError).Stderr))
			log.Fatalf("error running template command: %v\n", err)
		}
	}

	renderedPath, err := transpileConfig(templateContent)
	if err != nil {
		log.Fatalf("error transpiling config: %v\n", err)
	}

	defer func(path string) {
		if err := os.Remove(path); err != nil {
			log.Fatalf("error removing tempfile: %v\n", err)
		}
	}(renderedPath)

	// enable rescue boot
	if !server.RescueEnabled {
		log.Println("enabling rescue boot")
		result, _, err := client.Server.EnableRescue(context.Background(), server, hcloud.ServerEnableRescueOpts{
			Type:    hcloud.ServerRescueTypeLinux64,
			SSHKeys: []*hcloud.SSHKey{sshKey},
		})
		if err != nil {
			log.Fatalf("error sending enablerescue request: %v\n", err)
		}
		if result.Action.Error() != nil {
			log.Fatalf("error enabling rescue: %v\n", result.Action.Error())
		}

		err = waitForAction(client.Action, result.Action)
		if err != nil {
			log.Fatalf("error waiting for action: %v\n", err)
		}
	}

	var action *hcloud.Action
	if server.Status == hcloud.ServerStatusRunning {
		// server is already running, reboot into rescue
		log.Println("server already running, rebooting into rescue for reinstall")
		action, _, err = client.Server.Reboot(context.Background(), server)
	} else {
		log.Printf("powering server on")
		action, _, err = client.Server.Poweron(context.Background(), server)
	}
	if err != nil {
		log.Fatalf("error sending reboot or poweron request: %v\n", err)
	}
	if action.Error() != nil {
		log.Fatalf("error rebooting or powering on server: %v\n", action.Error())
	}

	err = waitForAction(client.Action, action)
	if err != nil {
		log.Fatalf("error waiting for action: %v\n", err)
	}

	// give the server some time to (re)boot
	log.Println("sleeping 30s to wait for server to (re)boot into rescue")
	time.Sleep(30 * time.Second)

	var sshAuth goph.Auth
	if cfg.HCloud.SSHKeyPrivatePath != "" {
		sshAuth, err = goph.Key(cfg.HCloud.SSHKeyPrivatePath, "")
	} else {
		sshAuth, err = goph.UseAgent()
	}
	if err != nil {
		log.Fatalf("error building ssh authentication: %v\n", err)
	}

	initialRetries := 30
	retries := 1
	connectionSuccess := false
	retryDelay := 10 * time.Second
	var sshClient *goph.Client
	for retries <= initialRetries {
		// TODO: add option to enable host key checking, will be random, though because rescue always has a different hostkey
		sshClient, err = goph.NewUnknown("root", server.PublicNet.IPv4.IP.String(), sshAuth)
		if err == nil {
			connectionSuccess = true
			break
		} else {
			if netError, ok := err.(net.Error); ok {
				log.Printf("retrying network error (%d/%d): %v\n", retries, initialRetries, netError)
				retries++
				time.Sleep(retryDelay)
			} else {
				log.Fatalf("unretriable error while etablishing ssh connection: %v\n", err)
				break
			}
		}
	}

	if !connectionSuccess {
		log.Fatalf("ssh connection wasn't successful")
	}

	// Defer closing the network connection.
	defer sshClient.Close()

	installScriptTarget := "/root/flatcar-install"
	ignitionTarget := "/root/ignition.json"

	if cfg.Flatcar.InstallScript != "" {
		err = sshClient.Upload(cfg.Flatcar.InstallScript, installScriptTarget)
		if err != nil {
			log.Fatalf("error uploading flatcar-install script: %v\n", err)
		}
	} else {
		// download install script on remote maschine
		cmd, err := sshClient.Command(fmt.Sprintf("curl -sS -o %s %s", installScriptTarget, installScriptSource))
		if err != nil {
			log.Fatalf("error creating cmd for install script download: %v\n", err)
		}
		err = cmd.Run()
		if err != nil {
			log.Fatalf("error downloading install script: %v\n", err)
		}
	}
	err = sshClient.Upload(renderedPath, ignitionTarget)
	if err != nil {
		log.Fatalf("error uploading ignition file: %v\n", err)
	}

	// build flatcar-install command
	var installDeviceArg string
	if cfg.Flatcar.InstallDevice == "" {
		installDeviceArg = "-s"
	} else {
		installDeviceArg = fmt.Sprintf("-d %s", cfg.Flatcar.InstallDevice)
	}
	installCommand := fmt.Sprintf("%s -i %s -V %s %s %s", installScriptTarget, ignitionTarget, cfg.Flatcar.Version, installDeviceArg, cfg.Flatcar.InstallArgs)

	// execute commands to finally install flatcar
	commands := []string{
		"apt update",
		"apt install -y gawk",
		fmt.Sprintf("chmod +x %s", installScriptTarget),
		installCommand,
	}
	for _, command := range commands {
		log.Printf("running command '%s'\n", command)
		cmd, err := sshClient.Command(command)
		if err != nil {
			log.Fatalf("error creating goph.Cmd for '%s': %v\n", command, err)
		}
		stdoutPipe, err := cmd.StdoutPipe()
		if err != nil {
			log.Fatalf("error creating stdoutpipe for '%s': %v\n", command, err)
		}
		go func() {
			// TODO: don't print this if not desired
			scanner := bufio.NewScanner(stdoutPipe)
			for scanner.Scan() {
				log.Printf("%s - %s", command, scanner.Text())
			}
		}()
		err = cmd.Run()
		if err != nil {
			log.Fatalf("error running command '%s': %v\n", command, err)
		}
	}

	// run reboot command
	cmd, err := sshClient.Command("reboot now")
	if err != nil {
		log.Fatalf("error creating goph.Cmd for reboot command: %v\n", err)
	}
	err = cmd.Run()
	if err != nil {
		log.Printf("reboot command failed, VM probably rebooted anyways: %v\n", err)
	}

	log.Println("------")
	log.Printf("successfully (re)installed %s, ID: %d IPv4: %s IPv6: %s\n", server.Name, server.ID, server.PublicNet.IPv4.IP.String(), server.PublicNet.IPv6.IP.String())
}
