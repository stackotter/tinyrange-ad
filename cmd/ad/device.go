package main

import (
	"fmt"
	"net"
	"strconv"
)

type DeviceConfig struct {
	ID     int
	UserID int
	Name   string
	Config *string
}

type WireguardDevice struct {
	ID        int
	UserID    int
	ConfigUrl string
	Name      string
	IP        string
}

type Device struct {
	game   *AttackDefenseGame
	wg     WireguardInstance
	name   string
	userID int
	id     int
	flows  []ParsedFlow
	tags   TagList
}

// implements FlowInstance.
func (d *Device) Flows() []ParsedFlow     { return d.flows }
func (d *Device) Hostname() string        { return "device_" + d.name }
func (d *Device) InstanceAddress() net.IP { return net.IPv4(10, 40, 30, 1+byte(d.id)) }
func (d *Device) Services() []FlowService { return []FlowService{} }
func (d *Device) Tags() TagList           { return d.tags }

func (d *Device) evaluateDeviceVariable(s string) (string, error) {
	if s == "team" {
		user, err := d.game.Persist.GetUser(d.userID)
		if err != nil {
			return "", fmt.Errorf("failed to get team from user id: %v", err)
		}
		return strconv.Itoa(user.TeamID), nil
	} else {
		return "", fmt.Errorf("invalid variable variable: %s", s)
	}
}

func (d *Device) ParseTags() error {
	tags, err := d.game.Config.Device.Tags.Parse(d.evaluateDeviceVariable)

	if err != nil {
		return fmt.Errorf("failed to parse tags for device (%s): %w", d.name, err)
	}

	d.tags = tags

	return nil
}

func (d *Device) ParseFlows() error {
	flows, err := d.game.Config.Device.Flows.Parse(d.evaluateDeviceVariable)
	if err != nil {
		return fmt.Errorf("failed to parse flows for device (%s): %w", d.name, err)
	}

	d.flows = flows

	return nil
}

var (
	_ FlowInstance = &Device{}
)
