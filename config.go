package main

import (
	"errors"

	"github.com/BurntSushi/toml"
)

type hcloudConfig struct {
	Token          string
	SSHKey         string `toml:"ssh_key"`
	PrivateNetwork string `toml:"private_network"`
	ServerType     string `toml:"server_type"`
	Datacenter     string
	Image          string
}

type flatcarConfig struct {
	Version        string
	ConfigTemplate string            `toml:"config_template"`
	TemplateStatic map[string]string `toml:"template_static"`
}

type config struct {
	HCloud  hcloudConfig
	Flatcar flatcarConfig
}

func verifyConfig(conf *config) error {
	if conf.HCloud.Token == "" {
		return errors.New("hcloud token missing")
	}
	if conf.HCloud.SSHKey == "" {
		return errors.New("ssh key missing")
	}
	if conf.HCloud.PrivateNetwork == "" {
		return errors.New("private network missing")
	}
	if conf.HCloud.ServerType == "" {
		return errors.New("server type missing")
	}
	if conf.HCloud.Datacenter == "" {
		return errors.New("datacenter missing")
	}
	if conf.HCloud.Image == "" {
		conf.HCloud.Image = "debian-11"
	}
	if conf.Flatcar.Version == "" {
		// TODO: set to latest version if not given
		return errors.New("flatcar version missing")
	}
	if conf.Flatcar.ConfigTemplate == "" {
		conf.Flatcar.ConfigTemplate = "ignition.yml.gtpl"
	}
	return nil
}

func ParseConfig(filename string) (config, error) {
	var conf config
	_, err := toml.DecodeFile(filename, &conf)
	if err != nil {
		return conf, err
	}
	err = verifyConfig(&conf)
	return conf, err
}