// Authorization server for DFC
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package main

import (
	"fmt"

	"github.com/NVIDIA/dfcpub/dfc"
	"github.com/NVIDIA/dfcpub/pkg/client"
	"github.com/golang/glog"
)

type (
	proxy struct {
		Url        string    `json:"url"`
		Smap       *dfc.Smap `json:"smap"`
		configPath string
	}
)

func NewProxy(configPath, defaultUrl string) *proxy {
	defProxy := &proxy{configPath: configPath, Url: defaultUrl}
	p := &proxy{}
	err := dfc.LocalLoad(configPath, p)
	if err != nil {
		glog.Errorf("Could not load configuration")
		return defProxy
	}

	err = p.detectPrimary()
	if err != nil {
		glog.Errorf("Failed to detect primary proxy: %v", err)
		return defProxy
	}

	p.configPath = configPath
	if p.Url != defaultUrl {
		p.saveSmap()
	}

	return p
}

func (p *proxy) saveSmap() {
	err := dfc.LocalSave(p.configPath, p)
	if err != nil {
		glog.Errorf("Failed to save configuration: %v", err)
	}
}

func (p *proxy) detectPrimary() error {
	if p.Smap == nil || len(p.Smap.Pmap)+len(p.Smap.Tmap) == 0 {
		return fmt.Errorf("Cluster map is empty")
	}

	// FIXME: it would be good to make a function that traverse daemon
	// list and call it with Pmap and Tmap but at this moment daemonInfo
	// struct is private, so there go two similar loops
	for _, pinfo := range p.Smap.Pmap {
		if pinfo.DirectURL == p.Url {
			continue
		}

		smap, err := client.GetClusterMap(pinfo.DirectURL)
		if err != nil {
			glog.Errorf("Failed to get cluster map: %v", err)
			continue
		}

		if smap.ProxySI.DirectURL != p.Url {
			p.Url = smap.ProxySI.DirectURL
			p.Smap = &smap
			return nil
		}
	}

	for _, tinfo := range p.Smap.Tmap {
		if tinfo.DirectURL == p.Url {
			continue
		}

		smap, err := client.GetClusterMap(tinfo.DirectURL)
		if err != nil {
			glog.Errorf("Failed to get cluster map: %v", err)
			continue
		}

		if smap.ProxySI.DirectURL != p.Url {
			p.Url = smap.ProxySI.DirectURL
			p.Smap = &smap
			return nil
		}
	}

	return fmt.Errorf("Detecting primary proxy failed")
}
