# hetzner-flatcar
A tool to deploy [Flatcar Linux](https://flatcar.og) on Hetzner Cloud.
Includes transpiling of [Container Linux Config](https://www.flatcar.org/docs/latest/provisioning/cl-config/) and reinstalling maschines on changes.

## Build
`go build .`

## Usage
* create a config named `config.toml` with the values described in [configuration](#configuration).
* create a container linux config template, see [template](#template) for details
* download flatcar-install script into cwd `wget https://raw.githubusercontent.com/flatcar-linux/init/flatcar-master/bin/flatcar-install`
* `./hetzner-flatcar hostname

## Configuration
```toml
[hcloud]
token = "<hetzner cloud token>"
server_type = "cx11"
datacenter = "nbg1"
ssh_key = "<name of ssh key used for rescue and passed to template>"
private_network = "<private network server is attached to>"

[flatcar]
version = "3139.2.0"
config_template = "ignition.yml.gtpl"
[flatcar.template_static]
nomad_version = "1.2.6"
consul_version = "1.11.4"
```

## Template
The [Container Linux Config](https://github.com/flatcar-linux/container-linux-config-transpiler/blob/flatcar-master/doc/configuration.md) template is rendered using [text/template](https://golang.org/pkg/text/template/) and is given this data:
* `Server` - [Server](https://pkg.go.dev/github.com/hetznercloud/hcloud-go/hcloud#Server) object as returned by Hetzner Cloud API
* `SSHKey` - [SSHKey](https://pkg.go.dev/github.com/hetznercloud/hcloud-go/hcloud#SSHKey) object of the SSH Key used for rescue boot
* `Static` - static data from [config](#configuration) option `flatcar.template_static` as `map[string]string`

Afterwards it's transpiled into a Ignition file.

## Deployment procedure
1. check whether vm with the name given as first parameter already exists
2. create VM (if not already exists)
3. render container linux config template with data from new or existing VM
4. transpile container linux config into ignition file
5. enable rescue boot on VM
6. Startup or reboot VM (into rescue)
7. upload flatcar-install script and rendered ignition config
8. call flatcar-install and reboot
