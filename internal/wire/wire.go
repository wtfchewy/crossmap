// Package wire defines the payloads exchanged over a Crossmap link. Both the
// agent and the host depend on it so the two sides cannot drift.
package wire

// Control operation names carried in link.Request.Op.
const (
	OpDescribe       = "describe"
	OpListDevices    = "list-devices"
	OpReadPairRecord = "read-pair-record"
	OpReadBUID       = "read-buid"
	OpListServices   = "list-services"
)

// AgentInfo describes the machine an iPhone is physically plugged into.
type AgentInfo struct {
	Hostname  string `json:"hostname"`
	TailnetIP string `json:"tailnet_ip,omitempty"`
	OS        string `json:"os"`
	Version   string `json:"version"`
}

// DeviceInfo is one iOS device as the agent sees it.
//
// Devices are addressed by UDID rather than usbmuxd's DeviceID because the
// DeviceID is assigned per-host and changes on every replug; the UDID is
// stable, which keeps host-side state valid across agent restarts.
type DeviceInfo struct {
	Udid            string `json:"udid"`
	DeviceID        int    `json:"device_id"`
	ConnectionType  string `json:"connection_type"`
	ProductID       int    `json:"product_id,omitempty"`
	LocationID      int    `json:"location_id,omitempty"`
	ConnectionSpeed int    `json:"connection_speed,omitempty"`
	Agent           string `json:"agent,omitempty"`
}

// ListDevicesResult answers OpListDevices.
type ListDevicesResult struct {
	Devices []DeviceInfo `json:"devices"`
}

// ReadPairRecordArgs selects a device for OpReadPairRecord.
type ReadPairRecordArgs struct {
	Udid string `json:"udid"`
}

// ReadPairRecordResult carries a device's trust record as stored by the
// agent's usbmuxd. Transporting it lets the host complete a lockdown
// handshake without ever being paired with the device itself.
type ReadPairRecordResult struct {
	Data []byte `json:"data"`
}

// ReadBUIDResult answers OpReadBUID with the agent's system BUID, which must
// match the pair records the agent hands out.
type ReadBUIDResult struct {
	BUID string `json:"buid"`
}

// EventType distinguishes device arrival from departure.
type EventType string

const (
	EventAttached EventType = "attached"
	EventDetached EventType = "detached"
)

// DeviceEvent is pushed on a KindEvents stream whenever the agent's device
// set changes.
type DeviceEvent struct {
	Type   EventType  `json:"type"`
	Device DeviceInfo `json:"device"`
}

// Service is a CoreDevice RemotePairing advertisement the agent discovered on
// a device's USB network interface. Republishing it verbatim on the host is
// what makes Xcode and devicectl believe the device is locally present.
type Service struct {
	// Name is the Bonjour instance name, which encodes the device identity
	// and must be reproduced exactly for the host's remotepairingd to match
	// it against an existing pairing.
	Name string `json:"name"`
	// Type is normally "_remotepairing._tcp".
	Type string `json:"type"`
	// Domain is normally "local".
	Domain string `json:"domain"`
	// Host is the SRV target, e.g. "iPhone.local".
	Host string `json:"host"`
	// Port is the CoreDevice front door, conventionally 49152.
	Port int `json:"port"`
	// TXT carries the record's key/value pairs in advertisement order.
	TXT []string `json:"txt,omitempty"`
	// Interface is the agent-side interface the service was seen on, which
	// for a USB-attached iPhone is its CDC-NCM ethernet interface.
	Interface string `json:"interface,omitempty"`
	// Addrs are the addresses the agent can reach the service on. For USB
	// these are IPv6 link-local and therefore carry a %zone suffix.
	Addrs []string `json:"addrs,omitempty"`
	// Udid is filled in when the service can be matched to a usbmuxd device.
	Udid string `json:"udid,omitempty"`
}

// ListServicesResult answers OpListServices.
type ListServicesResult struct {
	Services []Service `json:"services"`
}
