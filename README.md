# hetzner-flatcar
A tool to deploy [Flatcar Linux](https://flatcar.og) on Hetzner Cloud.
Includes transpiling of [Container Linux Config](https://www.flatcar.org/docs/latest/provisioning/cl-config/) and reinstalling maschines on changes.

## Build
`go build .`

## Usage
* create a config named `config.toml` with the values described in [configuration](#configuration).
* create a container linux config template, see [template](#template) for details
* `./hetzner-flatcar hostname`

This tool will establish a SSH session to the rescue os to run the flatcar-install script using [goph](https://github.com/melbahja/goph).
For authentication it uses the SSH agent, so ensure the private counterpart to the public key uploaded to Hetzner and referenced in the config is added to your SSH agent.

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
# provide path to custom flatcar-install script
# if not provided will be downloaded from
# https://github.com/flatcar-linux/init/blob/flatcar-master/bin/flatcar-install
# install_script = "custom-install-script"
[flatcar.template_static]
nomad_version = "1.2.6"
consul_version = "1.11.4"
```

## Template
The [Container Linux Config](https://github.com/flatcar-linux/container-linux-config-transpiler/blob/flatcar-master/doc/configuration.md) template is rendered using [text/template](https://golang.org/pkg/text/template/) and is given this data:
* `Server` - [Server](https://pkg.go.dev/github.com/hetznercloud/hcloud-go/hcloud#Server) object as returned by Hetzner Cloud API
* `SSHKey` - [SSHKey](https://pkg.go.dev/github.com/hetznercloud/hcloud-go/hcloud#SSHKey) object of the SSH Key used for rescue boot
* `Static` - static data from [config](#configuration) option `flatcar.template_static` as `map[string]string`
* `ReadFile(filename string) (string, error)` - function to read a local file
* `Function(indent int, input string) string` - function to indent strings

Afterwards it's transpiled into a Ignition file.

Take a look at the [example config](doc/example.yml.gtpl) for a minimal example just creating a `core` user with the SSH Key used for rescue boot and setting the hostname to the maschine name.

### injecting local files
The container linux config transpiler supports injecting local files ([ref](https://github.com/flatcar-linux/container-linux-config-transpiler/blob/flatcar-master/config/types/files.go#L177)).
Unfortunately that feature is not usable when not calling it using the CLI, because it relies on the value of a flag to determine the base path to search for files.
As an alternative hetzner-flatcar supports the `ReadFile` template function to inject files into templates.

Example usage:
```
storage:
  files:
    - path: /etc/LICENSE
      filesystem: root
      contents:
        inline: |
{{ call .ReadFile "LICENSE" | call .Indent 12 }}
```

## Deployment procedure
1. check whether vm with the name given as first parameter already exists
2. create VM (if not already exists)
3. render container linux config template with data from new or existing VM
4. transpile container linux config into ignition file
5. enable rescue boot on VM
6. Startup or reboot VM (into rescue)
7. upload flatcar-install script and rendered ignition config
8. call flatcar-install and reboot
