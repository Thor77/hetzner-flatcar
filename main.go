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
	"path/filepath"
	"text/template"
	"time"

	clconfig "github.com/flatcar-linux/container-linux-config-transpiler/config"
	"github.com/hetznercloud/hcloud-go/hcloud"
	"github.com/melbahja/goph"
)

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
	Server hcloud.Server
	SSHKey hcloud.SSHKey
	Static map[string]string
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

	serverTypeName := cfg.HCloud.ServerType
	datacenterName := cfg.HCloud.Datacenter
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
		serverType, _, err := client.ServerType.GetByName(context.Background(), serverTypeName)
		if err != nil {
			log.Fatalf("error finding server type: %v\n", err)
		}
		image, _, err := client.Image.Get(context.Background(), cfg.HCloud.Image)
		if err != nil {
			log.Fatalf("error finding image: %v\n", err)
		}
		datacenter, _, err := client.Datacenter.GetByName(context.Background(), datacenterName)
		createOpts := hcloud.ServerCreateOpts{
			Name:             serverName,
			StartAfterCreate: &startAfterCreate,
			ServerType:       serverType,
			Image:            image,
			Datacenter:       datacenter,
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

	// render container linux config template
	// TODO: support name replacement in template path
	ignitionTemplate := cfg.Flatcar.ConfigTemplate
	buffer := &bytes.Buffer{}
	tmpl, err := template.New(filepath.Base(ignitionTemplate)).ParseFiles(ignitionTemplate)
	if err != nil {
		log.Fatalf("error loading template: %v\n", err)
	}
	err = tmpl.Execute(buffer, templateData{
		Server: *server,
		SSHKey: *sshKey,
		Static: cfg.Flatcar.TemplateStatic,
	})
	if err != nil {
		log.Fatalf("error rendering template: %v\n", err)
	}

	// transpile rendered container linux config template into ignition
	bufferContent, _ := ioutil.ReadAll(buffer)

	renderedPath, err := transpileConfig(bufferContent)
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

	sshAuth, err := goph.UseAgent()
	if err != nil {
		log.Fatal(err)
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

	// TODO: support different source for flatcar-install script
	err = sshClient.Upload("flatcar-install", "/root/flatcar-install")
	if err != nil {
		log.Fatalf("error uploading flatcar-install script: %v\n", err)
	}
	err = sshClient.Upload(renderedPath, "/root/ignition.json")
	if err != nil {
		log.Fatalf("error uploading ignition file: %v\n", err)
	}

	// execute commands to finally install flatcar
	commands := []string{
		"apt update",
		"apt install -y gawk",
		"chmod +x /root/flatcar-install",
		fmt.Sprintf("/root/flatcar-install -s -i /root/ignition.json -V %s", cfg.Flatcar.Version),
		"shutdown -r now",
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

	log.Println("------")
	log.Printf("successfully (re)installed %s, ID: %d IPv4: %s IPv6: %s\n", server.Name, server.ID, server.PublicNet.IPv4.IP.String(), server.PublicNet.IPv6.IP.String())
}